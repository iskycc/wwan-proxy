package webui

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"runtime"
	runtimemetrics "runtime/metrics"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"wwan-proxy/internal/config"
	"wwan-proxy/internal/manager"
	"wwan-proxy/internal/store"
)

//go:embed static/*
var assets embed.FS

type Server struct {
	store               *store.Store
	manager             *manager.Manager
	log                 *slog.Logger
	started             time.Time
	http                *http.Server
	limiter             *loginLimiter
	startupWebListen    string
	startupDatabasePath string
	websocketContext    context.Context
	websocketCancel     context.CancelFunc
	websocketInterval   time.Duration
	websocketClients    atomic.Int64
	// initialLiveHeap keeps the dashboard stable until the runtime completes its first GC cycle.
	initialLiveHeap atomic.Uint64
}

func New(address string, st *store.Store, mgr *manager.Manager, logger *slog.Logger) *Server {
	startupWebListen := address
	if settings, err := st.SystemSettings(context.Background()); err == nil {
		startupWebListen = settings.WebListen
	}
	websocketContext, websocketCancel := context.WithCancel(context.Background())
	s := &Server{store: st, manager: mgr, log: logger.With("component", "webui"), started: time.Now(), limiter: newLoginLimiter(), startupWebListen: startupWebListen, startupDatabasePath: st.Path(), websocketContext: websocketContext, websocketCancel: websocketCancel, websocketInterval: 2 * time.Second}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/auth/status", s.authStatus)
	mux.HandleFunc("POST /api/auth/initialize", s.initializeAdmin)
	mux.HandleFunc("POST /api/auth/login", s.login)
	mux.HandleFunc("POST /api/auth/logout", s.logout)
	mux.HandleFunc("PUT /api/admin", s.updateAdmin)
	mux.HandleFunc("GET /api/sessions", s.listSessions)
	mux.HandleFunc("DELETE /api/sessions/{id}", s.revokeSession)
	mux.HandleFunc("POST /api/sessions/revoke-others", s.revokeOtherSessions)
	mux.HandleFunc("GET /api/settings", s.getSettings)
	mux.HandleFunc("PUT /api/settings", s.saveSettings)
	mux.HandleFunc("GET /api/overview", s.overview)
	mux.HandleFunc("GET /api/ws", s.overviewWebSocket)
	mux.HandleFunc("GET /api/servers", s.listServers)
	mux.HandleFunc("POST /api/servers", s.saveServer)
	mux.HandleFunc("PUT /api/servers/{id}", s.saveServer)
	mux.HandleFunc("DELETE /api/servers/{id}", s.deleteServer)
	mux.HandleFunc("POST /api/servers/{id}/toggle", s.toggleServer)
	mux.HandleFunc("GET /api/logs", s.logs)
	mux.HandleFunc("GET /api/health", func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, http.StatusOK, map[string]any{"ok": true}) })
	staticFS, _ := fs.Sub(assets, "static")
	mux.Handle("/", http.FileServer(http.FS(staticFS)))
	s.http = &http.Server{Addr: address, Handler: securityHeaders(s.authMiddleware(mux)), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second}
	return s
}

func (s *Server) ListenAndServe() error {
	s.log.Info("WebUI listening", "address", s.http.Addr)
	err := s.http.ListenAndServe()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func (s *Server) Shutdown(ctx context.Context) error {
	s.websocketCancel()
	return s.http.Shutdown(ctx)
}

func (s *Server) overview(w http.ResponseWriter, r *http.Request) {
	payload, err := s.overviewData(r.Context())
	if err != nil {
		s.internalError(w, r, "load overview", err)
		return
	}
	writeJSON(w, http.StatusOK, payload)
}

func (s *Server) overviewData(ctx context.Context) (map[string]any, error) {
	configs, err := s.store.ListServers(ctx)
	if err != nil {
		return nil, fmt.Errorf("load overview configurations: %w", err)
	}
	heartbeats, err := s.store.ListHeartbeats(ctx)
	if err != nil {
		return nil, fmt.Errorf("load heartbeat status: %w", err)
	}
	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)
	liveHeapSample := []runtimemetrics.Sample{{Name: "/gc/heap/live:bytes"}}
	runtimemetrics.Read(liveHeapSample)
	var liveHeap uint64
	if liveHeapSample[0].Value.Kind() == runtimemetrics.KindUint64 {
		liveHeap = liveHeapSample[0].Value.Uint64()
	}
	if liveHeap == 0 {
		s.initialLiveHeap.CompareAndSwap(0, mem.HeapAlloc)
		liveHeap = s.initialLiveHeap.Load()
	}
	return map[string]any{
		"uptime_seconds": int64(time.Since(s.started).Seconds()), "servers": configs, "instances": s.manager.Snapshots(), "heartbeats": heartbeats,
		"process": map[string]any{"goroutines": runtime.NumGoroutine(), "heap_bytes": mem.HeapAlloc, "heap_live_bytes": liveHeap, "sys_bytes": mem.Sys, "gc_cycles": mem.NumGC, "websocket_clients": s.websocketClients.Load()},
	}, nil
}

var (
	errWebSocketSessionExpired   = errors.New("websocket session expired")
	errUnsupportedSocketMessage = errors.New("unsupported websocket message")
)

func (s *Server) overviewWebSocket(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "invalid request origin"})
		return
	}
	tokenHash := currentSessionHash(r)
	if tokenHash == "" {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "authentication required"})
		return
	}
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{CompressionMode: websocket.CompressionDisabled})
	if err != nil {
		s.log.Debug("WebSocket handshake failed", "remote", r.RemoteAddr, "error", err)
		return
	}
	conn.SetReadLimit(64)
	s.websocketClients.Add(1)
	defer s.websocketClients.Add(-1)
	defer conn.Close(websocket.StatusNormalClosure, "")

	// The HTTP request context is not valid after the connection is hijacked.
	ctx, cancel := context.WithCancel(s.websocketContext)
	defer cancel()
	refresh := make(chan struct{}, 1)
	readError := make(chan error, 1)
	go func() {
		for {
			messageType, message, readErr := conn.Read(ctx)
			if readErr != nil {
				readError <- readErr
				return
			}
			if messageType != websocket.MessageText || string(message) != "refresh" {
				_ = conn.Close(websocket.StatusPolicyViolation, "unsupported message")
				readError <- errUnsupportedSocketMessage
				return
			}
			select {
			case refresh <- struct{}{}:
			default:
			}
		}
	}()

	send := func() error {
		_, valid, validateErr := s.store.ValidateSession(ctx, tokenHash, time.Now())
		if validateErr != nil {
			return fmt.Errorf("validate WebSocket session: %w", validateErr)
		}
		if !valid {
			return errWebSocketSessionExpired
		}
		payload, dataErr := s.overviewData(ctx)
		if dataErr != nil {
			return dataErr
		}
		writeContext, writeCancel := context.WithTimeout(ctx, 5*time.Second)
		defer writeCancel()
		return wsjson.Write(writeContext, conn, payload)
	}
	handleSendError := func(sendErr error) bool {
		if sendErr == nil {
			return false
		}
		if errors.Is(sendErr, errWebSocketSessionExpired) {
			_ = conn.Close(websocket.StatusPolicyViolation, "session expired")
			return true
		}
		if ctx.Err() == nil && websocket.CloseStatus(sendErr) == -1 {
			s.log.Warn("WebSocket overview push failed", "remote", r.RemoteAddr, "error", sendErr)
		}
		return true
	}
	if handleSendError(send()) {
		return
	}

	ticker := time.NewTicker(s.websocketInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case readErr := <-readError:
			status := websocket.CloseStatus(readErr)
			if status == -1 && ctx.Err() == nil && !errors.Is(readErr, errUnsupportedSocketMessage) {
				s.log.Debug("WebSocket connection ended", "remote", r.RemoteAddr, "error", readErr)
			}
			return
		case <-refresh:
			if handleSendError(send()) {
				return
			}
		case <-ticker.C:
			if handleSendError(send()) {
				return
			}
		}
	}
}

func (s *Server) listServers(w http.ResponseWriter, r *http.Request) {
	configs, err := s.store.ListServers(r.Context())
	if err != nil {
		s.internalError(w, r, "list configurations", err)
		return
	}
	writeJSON(w, http.StatusOK, configs)
}

func (s *Server) saveServer(w http.ResponseWriter, r *http.Request) {
	defer r.Body.Close()
	dec := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	dec.DisallowUnknownFields()
	var cfg config.Server
	if err := dec.Decode(&cfg); err != nil {
		s.log.Warn("invalid configuration request", "remote", r.RemoteAddr, "error", err)
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request: " + err.Error()})
		return
	}
	if r.Method == http.MethodPut {
		id, err := parseID(r.PathValue("id"))
		if err != nil {
			s.log.Warn("invalid server id", "remote", r.RemoteAddr, "error", err)
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		cfg.ID = id
	} else {
		cfg.ID = 0
	}
	if err := s.store.SaveServer(r.Context(), &cfg); err != nil {
		s.log.Warn("save server configuration rejected", "server", cfg.Name, "interface", cfg.Interface, "remote", r.RemoteAddr, "error", err)
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if err := s.manager.Reload(r.Context(), cfg.ID); err != nil {
		s.internalError(w, r, "reload server configuration", err)
		return
	}
	s.log.Info("server configuration saved", "server", cfg.Name, "interface", cfg.Interface, "id", cfg.ID, "enabled", cfg.Enabled)
	writeJSON(w, http.StatusOK, cfg)
}

func (s *Server) deleteServer(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r.PathValue("id"))
	if err != nil {
		s.log.Warn("invalid server id", "remote", r.RemoteAddr, "error", err)
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	cfg, err := s.store.GetServer(r.Context(), id)
	if err != nil {
		s.log.Warn("delete requested for unknown server", "id", id, "remote", r.RemoteAddr, "error", err)
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "server not found"})
		return
	}
	s.manager.Remove(id)
	if err := s.store.DeleteServer(r.Context(), id); err != nil {
		s.internalError(w, r, "delete server configuration", err)
		return
	}
	s.log.Warn("server configuration deleted", "server", cfg.Name, "interface", cfg.Interface, "id", id)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) toggleServer(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r.PathValue("id"))
	if err != nil {
		s.log.Warn("invalid server id", "remote", r.RemoteAddr, "error", err)
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	cfg, err := s.store.GetServer(r.Context(), id)
	if err != nil {
		s.log.Warn("toggle requested for unknown server", "id", id, "remote", r.RemoteAddr, "error", err)
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "server not found"})
		return
	}
	cfg.Enabled = !cfg.Enabled
	if err := s.store.SaveServer(r.Context(), &cfg); err != nil {
		s.internalError(w, r, "save server toggle", err)
		return
	}
	if err := s.manager.Reload(r.Context(), id); err != nil {
		s.internalError(w, r, "reload toggled server", err)
		return
	}
	s.log.Info("server toggled", "server", cfg.Name, "interface", cfg.Interface, "enabled", cfg.Enabled)
	writeJSON(w, http.StatusOK, cfg)
}

func (s *Server) logs(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	logs, err := s.store.ListLogs(r.Context(), limit, r.URL.Query().Get("level"), r.URL.Query().Get("q"))
	if err != nil {
		s.internalError(w, r, "query logs", err)
		return
	}
	writeJSON(w, http.StatusOK, logs)
}

func parseID(value string) (int64, error) {
	id, err := strconv.ParseInt(value, 10, 64)
	if err != nil || id <= 0 {
		return 0, errors.New("invalid server id")
	}
	return id, nil
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
func (s *Server) internalError(w http.ResponseWriter, r *http.Request, operation string, err error) {
	s.log.Error("Web API operation failed", "operation", operation, "path", r.URL.Path, "remote", r.RemoteAddr, "error", err)
	writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'self' 'unsafe-inline'; script-src 'self'; img-src 'self' data:; connect-src 'self'; form-action 'self'; base-uri 'none'; object-src 'none'")
		if r.TLS != nil {
			w.Header().Set("Strict-Transport-Security", "max-age=31536000")
		}
		if strings.HasPrefix(r.URL.Path, "/api/") {
			w.Header().Set("Cache-Control", "no-store")
		}
		next.ServeHTTP(w, r)
	})
}

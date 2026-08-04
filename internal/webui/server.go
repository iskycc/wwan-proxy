package webui

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"runtime"
	runtimemetrics "runtime/metrics"
	"strconv"
	"strings"
	"time"

	"wwan-proxy/internal/config"
	"wwan-proxy/internal/manager"
	"wwan-proxy/internal/store"
)

//go:embed static/*
var assets embed.FS

type Server struct {
	store   *store.Store
	manager *manager.Manager
	log     *slog.Logger
	started time.Time
	http    *http.Server
	limiter *loginLimiter
}

func New(address string, st *store.Store, mgr *manager.Manager, logger *slog.Logger) *Server {
	s := &Server{store: st, manager: mgr, log: logger.With("component", "webui"), started: time.Now(), limiter: newLoginLimiter()}
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

func (s *Server) Shutdown(ctx context.Context) error { return s.http.Shutdown(ctx) }

func (s *Server) overview(w http.ResponseWriter, r *http.Request) {
	configs, err := s.store.ListServers(r.Context())
	if err != nil {
		s.internalError(w, r, "load overview configurations", err)
		return
	}
	heartbeats, err := s.store.ListHeartbeats(r.Context())
	if err != nil {
		s.internalError(w, r, "load heartbeat status", err)
		return
	}
	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)
	liveHeapSample := []runtimemetrics.Sample{{Name: "/gc/heap/live:bytes"}}
	runtimemetrics.Read(liveHeapSample)
	var liveHeap uint64
	if liveHeapSample[0].Value.Kind() == runtimemetrics.KindUint64 {
		liveHeap = liveHeapSample[0].Value.Uint64()
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"uptime_seconds": int64(time.Since(s.started).Seconds()), "servers": configs, "instances": s.manager.Snapshots(), "heartbeats": heartbeats,
		"process": map[string]any{"goroutines": runtime.NumGoroutine(), "heap_bytes": mem.HeapAlloc, "heap_live_bytes": liveHeap, "sys_bytes": mem.Sys, "gc_cycles": mem.NumGC},
	})
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

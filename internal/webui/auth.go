package webui

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"golang.org/x/crypto/bcrypt"

	"wwan-proxy/internal/store"
)

const (
	sessionCookieName = "wwan_session"
)

type authContextKey struct{}

type authRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

func (s *Server) authStatus(w http.ResponseWriter, r *http.Request) {
	initialized, err := s.store.AdminInitialized(r.Context())
	if err != nil {
		s.internalError(w, r, "check administrator initialization", err)
		return
	}
	username, expiresAt, authenticated := s.session(r)
	status := map[string]any{"initialized": initialized, "authenticated": authenticated, "username": username}
	if authenticated {
		status["expires_at"] = expiresAt
	}
	writeJSON(w, http.StatusOK, status)
}

func (s *Server) initializeAdmin(w http.ResponseWriter, r *http.Request) {
	req, ok := decodeAuthRequest(w, r)
	if !ok {
		return
	}
	if err := validateCredentials(req); err != nil {
		s.log.Warn("administrator initialization rejected", "remote", clientIP(r), "error", err)
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(req.Password), 12)
	if err != nil {
		s.internalError(w, r, "hash administrator password", err)
		return
	}
	if err := s.store.CreateAdmin(r.Context(), strings.TrimSpace(req.Username), hash); err != nil {
		if errors.Is(err, store.ErrAlreadyInitialized) {
			writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
			return
		}
		s.internalError(w, r, "create administrator", err)
		return
	}
	expiresAt, err := s.issueSession(w, r)
	if err != nil {
		s.internalError(w, r, "create initial session", err)
		return
	}
	s.log.Info("administrator initialized", "username", strings.TrimSpace(req.Username), "remote", clientIP(r))
	writeJSON(w, http.StatusCreated, map[string]any{"authenticated": true, "username": strings.TrimSpace(req.Username), "expires_at": expiresAt})
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	ip := clientIP(r)
	if allowed, retry := s.limiter.allow(ip); !allowed {
		w.Header().Set("Retry-After", strconvSeconds(retry))
		s.log.Warn("WebUI login rate limited", "remote", ip)
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "登录失败次数过多，请稍后重试"})
		return
	}
	req, ok := decodeAuthRequest(w, r)
	if !ok {
		return
	}
	admin, err := s.store.Admin(r.Context())
	if err != nil {
		s.limiter.failure(ip)
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "用户名或密码错误"})
		return
	}
	passwordValid := bcrypt.CompareHashAndPassword(admin.PasswordHash, []byte(req.Password)) == nil
	valid := strings.TrimSpace(req.Username) == admin.Username && passwordValid
	if !valid {
		s.limiter.failure(ip)
		s.log.Warn("WebUI login failed", "username", strings.TrimSpace(req.Username), "remote", ip)
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "用户名或密码错误"})
		return
	}
	s.limiter.success(ip)
	expiresAt, err := s.issueSession(w, r)
	if err != nil {
		s.internalError(w, r, "create login session", err)
		return
	}
	s.log.Info("WebUI login succeeded", "username", admin.Username, "remote", ip)
	writeJSON(w, http.StatusOK, map[string]any{"authenticated": true, "username": admin.Username, "expires_at": expiresAt})
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie(sessionCookieName); err == nil {
		_ = s.store.DeleteSession(r.Context(), hashToken(cookie.Value))
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookieName, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: r.TLS != nil})
	s.log.Info("WebUI logout", "remote", clientIP(r))
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) issueSession(w http.ResponseWriter, r *http.Request) (time.Time, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return time.Time{}, err
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	settings, err := s.store.SystemSettings(r.Context())
	if err != nil {
		return time.Time{}, err
	}
	lifetime := settings.SessionLifetime.Value(24 * time.Hour)
	expires := time.Now().Add(lifetime)
	ua := r.UserAgent()
	if len(ua) > 512 {
		ua = ua[:512]
	}
	if err := s.store.CreateSession(r.Context(), hashToken(token), clientIP(r), ua, expires); err != nil {
		return time.Time{}, err
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookieName, Value: token, Path: "/", Expires: expires, MaxAge: int(lifetime.Seconds()), HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: r.TLS != nil})
	return expires, nil
}

// refreshSessionCookie keeps the browser's cookie deadline aligned when the
// administrator changes the policy for sessions that already exist.
func (s *Server) refreshSessionCookie(w http.ResponseWriter, r *http.Request) {
	cookie, err := r.Cookie(sessionCookieName)
	if err != nil {
		return
	}
	_, expiresAt, valid, validateErr := s.store.ValidateSessionExpiry(r.Context(), hashToken(cookie.Value), time.Now())
	if validateErr != nil || !valid {
		http.SetCookie(w, &http.Cookie{Name: sessionCookieName, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: r.TLS != nil})
		return
	}
	remaining := time.Until(expiresAt)
	maxAge := int(remaining.Seconds())
	if maxAge < 1 {
		maxAge = 1
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookieName, Value: cookie.Value, Path: "/", Expires: expiresAt, MaxAge: maxAge, HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: r.TLS != nil})
}

func (s *Server) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isUnsafeMethod(r.Method) && !sameOrigin(r) {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "invalid request origin"})
			return
		}
		if !strings.HasPrefix(r.URL.Path, "/api/") || isPublicAPI(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		username, ok := s.sessionUser(r)
		if !ok {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "authentication required"})
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), authContextKey{}, username)))
	})
}

func isPublicAPI(path string) bool {
	switch path {
	case "/api/health", "/api/auth/status", "/api/auth/initialize", "/api/auth/login", "/api/auth/logout":
		return true
	default:
		return false
	}
}

func currentSessionHash(r *http.Request) string {
	cookie, err := r.Cookie(sessionCookieName)
	if err != nil {
		return ""
	}
	return hashToken(cookie.Value)
}

func (s *Server) sessionUser(r *http.Request) (string, bool) {
	username, _, valid := s.session(r)
	return username, valid
}

func (s *Server) session(r *http.Request) (string, time.Time, bool) {
	cookie, err := r.Cookie(sessionCookieName)
	if err != nil || len(cookie.Value) < 32 {
		return "", time.Time{}, false
	}
	username, expiresAt, ok, err := s.store.ValidateSessionExpiry(r.Context(), hashToken(cookie.Value), time.Now())
	return username, expiresAt, ok && err == nil
}

func decodeAuthRequest(w http.ResponseWriter, r *http.Request) (authRequest, bool) {
	defer r.Body.Close()
	var req authRequest
	dec := json.NewDecoder(io.LimitReader(r.Body, 16*1024))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请求格式错误"})
		return req, false
	}
	return req, true
}

func validateCredentials(req authRequest) error {
	username := strings.TrimSpace(req.Username)
	if utf8.RuneCountInString(username) < 3 || utf8.RuneCountInString(username) > 64 {
		return errors.New("管理员用户名长度必须为 3–64 个字符")
	}
	if len(req.Password) < 12 {
		return errors.New("管理员密码至少需要 12 个字符")
	}
	if len(req.Password) > 72 {
		return errors.New("管理员密码不能超过 72 字节")
	}
	return nil
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err == nil {
		return host
	}
	return r.RemoteAddr
}
func isUnsafeMethod(method string) bool {
	return method != http.MethodGet && method != http.MethodHead && method != http.MethodOptions
}
func sameOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	return err == nil && strings.EqualFold(u.Host, r.Host)
}
func strconvSeconds(d time.Duration) string {
	seconds := int(d.Seconds())
	if seconds < 1 {
		seconds = 1
	}
	return fmt.Sprint(seconds)
}

type loginAttempt struct {
	failures                  int
	windowStart, blockedUntil time.Time
}
type loginLimiter struct {
	mu       sync.Mutex
	attempts map[string]loginAttempt
}

func newLoginLimiter() *loginLimiter { return &loginLimiter{attempts: make(map[string]loginAttempt)} }
func (l *loginLimiter) allow(ip string) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	a := l.attempts[ip]
	if time.Now().Before(a.blockedUntil) {
		return false, time.Until(a.blockedUntil)
	}
	return true, 0
}
func (l *loginLimiter) failure(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	a := l.attempts[ip]
	if a.windowStart.IsZero() || now.Sub(a.windowStart) > 5*time.Minute {
		a = loginAttempt{windowStart: now}
	}
	a.failures++
	if a.failures >= 5 {
		a.blockedUntil = now.Add(5 * time.Minute)
	}
	l.attempts[ip] = a
}
func (l *loginLimiter) success(ip string) { l.mu.Lock(); delete(l.attempts, ip); l.mu.Unlock() }

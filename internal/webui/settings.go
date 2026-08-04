package webui

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/crypto/bcrypt"

	"wwan-proxy/internal/config"
)

type settingsResponse struct {
	config.SystemSettings
	CurrentWebListen    string `json:"current_web_listen"`
	CurrentDatabasePath string `json:"current_database_path"`
	StartupDatabasePath string `json:"startup_database_path"`
	RestartRequired     bool   `json:"restart_required"`
}

func (s *Server) getSettings(w http.ResponseWriter, r *http.Request) {
	settings, err := s.store.SystemSettings(r.Context())
	if err != nil {
		s.internalError(w, r, "load system settings", err)
		return
	}
	if settings.DatabasePath == "" {
		settings.DatabasePath = s.store.Path()
	}
	writeJSON(w, http.StatusOK, s.settingsResponse(settings))
}

func (s *Server) saveSettings(w http.ResponseWriter, r *http.Request) {
	defer r.Body.Close()
	var settings config.SystemSettings
	dec := json.NewDecoder(io.LimitReader(r.Body, 64*1024))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&settings); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request: " + err.Error()})
		return
	}
	settings.DatabasePath = filepath.Clean(settings.DatabasePath)
	if err := settings.Validate(); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if settings.DatabasePath != s.store.Path() {
		if _, err := os.Stat(settings.DatabasePath); err == nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "目标数据库文件已存在，请选择一个尚不存在的路径"})
			return
		} else if !errors.Is(err, os.ErrNotExist) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "无法检查目标数据库路径: " + err.Error()})
			return
		}
	}
	if err := s.store.SaveSystemSettings(r.Context(), &settings); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	_ = s.store.PruneLogs(r.Context(), time.Now().AddDate(0, 0, -settings.LogRetentionDays))
	_ = s.store.PruneSessions(r.Context(), time.Now())
	s.log.Info("system settings saved", "remote", clientIP(r), "restart_required", s.settingsResponse(settings).RestartRequired)
	writeJSON(w, http.StatusOK, s.settingsResponse(settings))
}

func (s *Server) settingsResponse(settings config.SystemSettings) settingsResponse {
	return settingsResponse{
		SystemSettings:      settings,
		CurrentWebListen:    s.http.Addr,
		CurrentDatabasePath: s.store.Path(),
		StartupDatabasePath: s.store.BootstrapPath(),
		RestartRequired:     settings.WebListen != s.startupWebListen || filepath.Clean(settings.DatabasePath) != s.startupDatabasePath,
	}
}

type updateAdminRequest struct {
	CurrentPassword string `json:"current_password"`
	Username        string `json:"username"`
	NewPassword     string `json:"new_password"`
}

func (s *Server) updateAdmin(w http.ResponseWriter, r *http.Request) {
	defer r.Body.Close()
	var req updateAdminRequest
	dec := json.NewDecoder(io.LimitReader(r.Body, 16*1024))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请求格式错误"})
		return
	}
	admin, err := s.store.Admin(r.Context())
	if err != nil || bcrypt.CompareHashAndPassword(admin.PasswordHash, []byte(req.CurrentPassword)) != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "当前密码错误"})
		return
	}
	username := strings.TrimSpace(req.Username)
	if utf8.RuneCountInString(username) < 3 || utf8.RuneCountInString(username) > 64 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "管理员用户名长度必须为 3–64 个字符"})
		return
	}
	hash := admin.PasswordHash
	if req.NewPassword != "" {
		if err := validateCredentials(authRequest{Username: username, Password: req.NewPassword}); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		hash, err = bcrypt.GenerateFromPassword([]byte(req.NewPassword), 12)
		if err != nil {
			s.internalError(w, r, "hash updated administrator password", err)
			return
		}
	}
	if err := s.store.UpdateAdmin(r.Context(), username, hash); err != nil {
		s.internalError(w, r, "update administrator", err)
		return
	}
	_ = s.store.DeleteOtherSessions(r.Context(), currentSessionHash(r))
	s.log.Warn("administrator credentials updated", "username", username, "remote", clientIP(r))
	writeJSON(w, http.StatusOK, map[string]string{"username": username})
}

func (s *Server) listSessions(w http.ResponseWriter, r *http.Request) {
	_ = s.store.PruneSessions(r.Context(), time.Now())
	sessions, err := s.store.ListSessions(r.Context())
	if err != nil {
		s.internalError(w, r, "list WebUI sessions", err)
		return
	}
	current := currentSessionHash(r)
	result := make([]map[string]any, 0, len(sessions))
	for _, session := range sessions {
		result = append(result, map[string]any{
			"id": session.TokenHash, "created_at": session.CreatedAt, "expires_at": session.ExpiresAt,
			"remote_addr": session.RemoteAddr, "user_agent": session.UserAgent, "current": session.TokenHash == current,
		})
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) revokeSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	decoded, err := hex.DecodeString(id)
	if err != nil || len(decoded) != 32 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid session id"})
		return
	}
	if err := s.store.DeleteSession(r.Context(), id); err != nil {
		s.internalError(w, r, "revoke WebUI session", err)
		return
	}
	if id == currentSessionHash(r) {
		http.SetCookie(w, &http.Cookie{Name: sessionCookieName, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: r.TLS != nil})
	}
	s.log.Warn("WebUI session revoked", "remote", clientIP(r), "current", id == currentSessionHash(r))
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) revokeOtherSessions(w http.ResponseWriter, r *http.Request) {
	if err := s.store.DeleteOtherSessions(r.Context(), currentSessionHash(r)); err != nil {
		s.internalError(w, r, "revoke other WebUI sessions", err)
		return
	}
	s.log.Warn("other WebUI sessions revoked", "remote", clientIP(r))
	w.WriteHeader(http.StatusNoContent)
}

package webui

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"wwan-proxy/internal/config"
	"wwan-proxy/internal/selfupdate"
)

type updateController interface {
	Status(context.Context, bool, string) (selfupdate.Info, error)
	Trigger(context.Context, string) (selfupdate.Info, error)
}

func (s *Server) ConfigureUpdates(controller updateController) {
	s.updates = controller
}

func (s *Server) getUpdate(w http.ResponseWriter, r *http.Request) {
	if s.updates == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "更新服务尚未配置"})
		return
	}
	checkRemote := r.URL.Query().Get("refresh") == "1"
	downloadInterface, ok := s.resolveUpdateInterface(w, r, r.URL.Query().Get("interface"))
	if !ok {
		return
	}
	info, err := s.updates.Status(r.Context(), checkRemote, downloadInterface)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "检查更新失败: " + err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, info)
}

func (s *Server) startUpdate(w http.ResponseWriter, r *http.Request) {
	if s.updates == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "更新服务尚未配置"})
		return
	}
	defer r.Body.Close()
	var request struct {
		Interface string `json:"interface"`
	}
	decoder := json.NewDecoder(io.LimitReader(r.Body, 4096))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil && !errors.Is(err, io.EOF) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "更新请求格式无效: " + err.Error()})
		return
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "更新请求只能包含一个 JSON 对象"})
		return
	}
	downloadInterface, ok := s.resolveUpdateInterface(w, r, request.Interface)
	if !ok {
		return
	}
	info, err := s.updates.Trigger(r.Context(), downloadInterface)
	if err != nil {
		switch {
		case errors.Is(err, selfupdate.ErrNoUpdate):
			writeJSON(w, http.StatusConflict, map[string]string{"error": "当前已经是最新版本"})
		case errors.Is(err, selfupdate.ErrUpdateInProgress):
			writeJSON(w, http.StatusConflict, map[string]string{"error": "已有更新任务正在执行"})
		case errors.Is(err, selfupdate.ErrUpdateUnsupported):
			message := strings.TrimPrefix(err.Error(), selfupdate.ErrUpdateUnsupported.Error()+": ")
			writeJSON(w, http.StatusNotImplemented, map[string]string{"error": message})
		default:
			s.log.Error("schedule automatic update failed", "remote", clientIP(r), "error", err)
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": "启动更新失败: " + err.Error()})
		}
		return
	}
	s.log.Warn("automatic update scheduled", "remote", clientIP(r), "current_version", info.CurrentVersion, "target_version", info.Latest.Version, "download_interface", downloadInterface)
	writeJSON(w, http.StatusAccepted, map[string]any{"status": "scheduled", "target": info.Latest})
}

func (s *Server) resolveUpdateInterface(w http.ResponseWriter, r *http.Request, value string) (string, bool) {
	name := strings.TrimSpace(value)
	if name == "" {
		return "", true
	}
	configs, err := s.store.ListServers(r.Context())
	if err != nil {
		s.internalError(w, r, "load update interfaces", err)
		return "", false
	}
	if err := validateConfiguredUpdateInterface(name, configs); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return "", false
	}
	return name, true
}

func validateConfiguredUpdateInterface(name string, configs []config.Server) error {
	if err := selfupdate.ValidateDownloadInterface(name); err != nil {
		return err
	}
	for _, cfg := range configs {
		if cfg.Interface == name {
			return nil
		}
	}
	return errors.New("下载网口必须来自已保存的出口配置")
}

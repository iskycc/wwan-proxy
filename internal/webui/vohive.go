package webui

import (
	"net/http"
	"strconv"

	"wwan-proxy/internal/store"
)

func (s *Server) vohiveEvents(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	opts := store.ListVohiveEventsOptions{
		DeviceID: r.URL.Query().Get("device"),
		Type:     store.VohiveEventType(r.URL.Query().Get("type")),
	}
	if limit := r.URL.Query().Get("limit"); limit != "" {
		if n, err := strconv.Atoi(limit); err == nil && n > 0 {
			opts.Limit = n
		}
	}
	events, err := s.store.ListVohiveEvents(r.Context(), opts)
	if err != nil {
		s.internalError(w, r, "list vohive events", err)
		return
	}
	writeJSON(w, http.StatusOK, events)
}

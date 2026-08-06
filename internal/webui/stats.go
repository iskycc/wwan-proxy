package webui

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"wwan-proxy/internal/store"
)

type statsPoint struct {
	Bucket             string `json:"bucket"`
	UploadBytes        uint64 `json:"upload_bytes"`
	DownloadBytes      uint64 `json:"download_bytes"`
	TotalConnections   uint64 `json:"total_connections"`
	ConnectionErrors   uint64 `json:"connection_errors"`
	TotalRequests      uint64 `json:"total_requests"`
	RequestErrors      uint64 `json:"request_errors"`
	ActiveConnections  int64  `json:"active_connections"`
	HeartbeatLatencyMs int64  `json:"heartbeat_latency_ms"`
	HeartbeatHealthy   bool   `json:"heartbeat_healthy"`
}

func (s *Server) statsServers(w http.ResponseWriter, r *http.Request) {
	configs, err := s.store.ListServers(r.Context())
	if err != nil {
		s.internalError(w, r, "list stats servers", err)
		return
	}
	type serverRef struct {
		ID   int64  `json:"id"`
		Name string `json:"name"`
	}
	result := make([]serverRef, 0, len(configs))
	for _, cfg := range configs {
		result = append(result, serverRef{ID: cfg.ID, Name: cfg.Name})
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) stats(w http.ResponseWriter, r *http.Request) {
	opts, err := parseStatsQuery(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	rows, err := s.store.ListServerStats(r.Context(), opts)
	if err != nil {
		s.internalError(w, r, "list server stats", err)
		return
	}
	points := make([]statsPoint, len(rows))
	for i, r := range rows {
		points[i] = statsPoint{
			Bucket:             r.Bucket.UTC().Format(time.RFC3339),
			UploadBytes:        r.TCPUploadBytes + r.UDPUploadBytes + r.HTTPUploadBytes,
			DownloadBytes:      r.TCPDownloadBytes + r.UDPDownloadBytes + r.HTTPDownloadBytes,
			TotalConnections:   r.TotalConnections,
			ConnectionErrors:   r.ConnectionErrors,
			TotalRequests:      r.TotalRequests,
			RequestErrors:      r.RequestErrors,
			ActiveConnections:  r.ActiveConnections,
			HeartbeatLatencyMs: r.HeartbeatLatencyMs,
			HeartbeatHealthy:   r.HeartbeatHealthy,
		}
	}
	writeJSON(w, http.StatusOK, points)
}

func (s *Server) statsSummary(w http.ResponseWriter, r *http.Request) {
	serverID, from, to, err := parseStatsSummaryQuery(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	summary, err := s.store.ServerStatsSummary(r.Context(), serverID, from, to)
	if err != nil {
		s.internalError(w, r, "server stats summary", err)
		return
	}
	writeJSON(w, http.StatusOK, summary)
}

func parseStatsQuery(r *http.Request) (store.ListServerStatsOptions, error) {
	var opts store.ListServerStatsOptions
	q := r.URL.Query()
	if v := q.Get("server_id"); v != "" {
		id, err := strconv.ParseInt(v, 10, 64)
		if err != nil || id <= 0 {
			return opts, errors.New("invalid server_id")
		}
		opts.ServerID = id
	}
	var err error
	opts.From, err = parseTimeParam(q.Get("from"))
	if err != nil {
		return opts, err
	}
	opts.To, err = parseTimeParam(q.Get("to"))
	if err != nil {
		return opts, err
	}
	opts.Step = q.Get("step")
	if opts.Step == "" {
		opts.Step = "minute"
	} else if opts.Step != "minute" && opts.Step != "hour" && opts.Step != "day" {
		return opts, errors.New("invalid step, expected minute, hour or day")
	}
	if !opts.From.IsZero() && !opts.To.IsZero() && opts.From.After(opts.To) {
		return opts, errors.New("from must be before to")
	}
	return opts, nil
}

func parseStatsSummaryQuery(r *http.Request) (serverID int64, from, to time.Time, err error) {
	q := r.URL.Query()
	if v := q.Get("server_id"); v != "" {
		serverID, err = strconv.ParseInt(v, 10, 64)
		if err != nil || serverID <= 0 {
			return 0, from, to, errors.New("invalid server_id")
		}
	}
	from, err = parseTimeParam(q.Get("from"))
	if err != nil {
		return 0, from, to, err
	}
	to, err = parseTimeParam(q.Get("to"))
	if err != nil {
		return 0, from, to, err
	}
	if !from.IsZero() && !to.IsZero() && from.After(to) {
		return 0, from, to, errors.New("from must be before to")
	}
	return serverID, from, to, nil
}

func parseTimeParam(value string) (time.Time, error) {
	if value == "" {
		return time.Time{}, nil
	}
	t, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return time.Time{}, errors.New("invalid time format, expected RFC3339")
	}
	return t, nil
}

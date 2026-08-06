package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

type VohiveEventType string

const (
	VohiveEventDegraded          VohiveEventType = "degraded"
	VohiveEventRecovered         VohiveEventType = "recovered"
	VohiveEventRecoveryStarted   VohiveEventType = "recovery_started"
	VohiveEventRecoverySucceeded VohiveEventType = "recovery_succeeded"
	VohiveEventRecoveryFailed    VohiveEventType = "recovery_failed"
)

type VohiveEvent struct {
	ID        int64           `json:"id"`
	Type      VohiveEventType `json:"type"`
	DeviceID  string          `json:"device_id"`
	ServerID  *int64          `json:"server_id,omitempty"`
	Message   string          `json:"message"`
	Details   map[string]any  `json:"details"`
	CreatedAt time.Time       `json:"created_at"`
}

type ListVohiveEventsOptions struct {
	DeviceID string
	Type     VohiveEventType
	Limit    int
	BeforeID int64
}

func (s *Store) SaveVohiveEvent(ctx context.Context, event VohiveEvent) (int64, error) {
	details, err := json.Marshal(event.Details)
	if err != nil {
		return 0, fmt.Errorf("marshal details: %w", err)
	}
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO vohive_events (type, device_id, server_id, message, details, created_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		string(event.Type), event.DeviceID, event.ServerID, event.Message, string(details), event.CreatedAt,
	)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) ListVohiveEvents(ctx context.Context, opts ListVohiveEventsOptions) ([]VohiveEvent, error) {
	if opts.Limit <= 0 {
		opts.Limit = 100
	}
	var clauses []string
	var args []any
	if opts.DeviceID != "" {
		clauses = append(clauses, "device_id = ?")
		args = append(args, opts.DeviceID)
	}
	if opts.Type != "" {
		clauses = append(clauses, "type = ?")
		args = append(args, string(opts.Type))
	}
	if opts.BeforeID > 0 {
		clauses = append(clauses, "id < ?")
		args = append(args, opts.BeforeID)
	}
	query := "SELECT id, type, device_id, server_id, message, details, created_at FROM vohive_events"
	if len(clauses) > 0 {
		query += " WHERE " + strings.Join(clauses, " AND ")
	}
	query += " ORDER BY id DESC LIMIT ?"
	args = append(args, opts.Limit)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var events []VohiveEvent
	for rows.Next() {
		var e VohiveEvent
		var details string
		var serverID sql.NullInt64
		if err := rows.Scan(&e.ID, &e.Type, &e.DeviceID, &serverID, &e.Message, &details, &e.CreatedAt); err != nil {
			return nil, err
		}
		if serverID.Valid {
			e.ServerID = &serverID.Int64
		}
		if details != "" {
			_ = json.Unmarshal([]byte(details), &e.Details)
		}
		events = append(events, e)
	}
	return events, rows.Err()
}

func (s *Store) PruneVohiveEvents(ctx context.Context, retention time.Duration) (int64, error) {
	cutoff := time.Now().UTC().Add(-retention)
	res, err := s.db.ExecContext(ctx, "DELETE FROM vohive_events WHERE created_at < ?", cutoff)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

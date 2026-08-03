package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"wwan-proxy/internal/config"
)

func (s *Store) SystemSettings(ctx context.Context) (config.SystemSettings, error) {
	var settings config.SystemSettings
	var raw string
	err := s.db.QueryRowContext(ctx, `SELECT settings_json FROM system_settings WHERE id=1`).Scan(&raw)
	if err == sql.ErrNoRows {
		settings.ApplyDefaults()
		return settings, nil
	}
	if err != nil {
		return settings, err
	}
	if err := json.Unmarshal([]byte(raw), &settings); err != nil {
		return settings, err
	}
	settings.ApplyDefaults()
	return settings, nil
}

func (s *Store) SaveSystemSettings(ctx context.Context, settings *config.SystemSettings) error {
	if err := settings.Validate(); err != nil {
		return err
	}
	raw, err := json.Marshal(settings)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO system_settings(id,settings_json,updated_at) VALUES(1,?,?)
ON CONFLICT(id) DO UPDATE SET settings_json=excluded.settings_json,updated_at=excluded.updated_at`,
		string(raw), time.Now().UTC().Format(time.RFC3339Nano))
	return err
}

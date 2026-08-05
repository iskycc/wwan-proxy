package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net"
	"time"

	"wwan-proxy/internal/config"
)

func (s *Store) SystemSettings(ctx context.Context) (config.SystemSettings, error) {
	return s.systemSettings(ctx, s.webListenDefault)
}

// SystemSettingsWithWebDefault applies webDefault only when SQLite does not
// contain an explicit WebUI listener. This lets service integrations choose a
// platform-appropriate first-run address without overriding a value saved by
// the administrator.
func (s *Store) SystemSettingsWithWebDefault(ctx context.Context, webDefault string) (config.SystemSettings, error) {
	if err := validateWebDefault(webDefault); err != nil {
		return config.SystemSettings{}, err
	}
	return s.systemSettings(ctx, webDefault)
}

// SetWebListenDefault sets the process-wide default used by all later reads
// from this Store. It must be called during startup, before the Store is shared
// with background goroutines.
func (s *Store) SetWebListenDefault(webDefault string) error {
	if err := validateWebDefault(webDefault); err != nil {
		return err
	}
	s.webListenDefault = webDefault
	return nil
}

func validateWebDefault(webDefault string) error {
	if webDefault == "" {
		return nil
	}
	if _, _, err := net.SplitHostPort(webDefault); err != nil {
		return fmt.Errorf("invalid WebUI default %q: %w", webDefault, err)
	}
	return nil
}

func (s *Store) systemSettings(ctx context.Context, webDefault string) (config.SystemSettings, error) {
	var settings config.SystemSettings
	var raw string
	err := s.db.QueryRowContext(ctx, `SELECT settings_json FROM system_settings WHERE id=1`).Scan(&raw)
	if err == sql.ErrNoRows {
		settings.WebListen = webDefault
		settings.ApplyDefaults()
		return settings, nil
	}
	if err != nil {
		return settings, err
	}
	if err := json.Unmarshal([]byte(raw), &settings); err != nil {
		return settings, err
	}
	if settings.WebListen == "" {
		settings.WebListen = webDefault
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

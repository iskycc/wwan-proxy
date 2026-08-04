package store

import (
	"context"
	"encoding/json"
	"fmt"

	"wwan-proxy/internal/config"
	"wwan-proxy/internal/proxyauth"
)

func (s *Store) prepareProxyCredentials(ctx context.Context, cfg *config.Server, trustStoredHashes bool) error {
	unchanged := make(map[string]struct{}, len(cfg.Auth.PasswordUnchanged))
	for _, user := range cfg.Auth.PasswordUnchanged {
		unchanged[user] = struct{}{}
	}
	legacyRedactionMarkers := cfg.Auth.PasswordUnchanged == nil
	var previous map[string]string
	if cfg.ID != 0 {
		old, err := s.GetServer(ctx, cfg.ID)
		if err != nil {
			return err
		}
		previous = old.Auth.Users
	}
	for user, password := range cfg.Auth.Users {
		_, explicitlyUnchanged := unchanged[user]
		preserved := false
		if explicitlyUnchanged || legacyRedactionMarkers && password == proxyauth.Redacted {
			password = previous[user]
			if password == "" {
				return fmt.Errorf("auth.users[%q] uses a redacted password without an existing credential", user)
			}
			preserved = true
		}
		if !preserved && (!trustStoredHashes || !proxyauth.IsHash(password)) {
			hash, err := proxyauth.Hash(password)
			if err != nil {
				return fmt.Errorf("hash proxy password for %q: %w", user, err)
			}
			password = hash
		}
		cfg.Auth.Users[user] = password
	}
	cfg.Auth.PasswordUnchanged = nil
	return nil
}

// migrateProxyCredentials upgrades legacy plaintext proxy credentials in the
// JSON configuration without changing the SQLite schema.
func (s *Store) migrateProxyCredentials(ctx context.Context) error {
	var cleanupPending bool
	if err := s.db.QueryRowContext(ctx, `SELECT cleanup_pending FROM security_migrations WHERE name='proxy_credentials_v1'`).Scan(&cleanupPending); err != nil {
		return err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id, config_json FROM server_configs`)
	if err != nil {
		return err
	}
	type update struct {
		id  int64
		raw string
	}
	var updates []update
	for rows.Next() {
		var id int64
		var raw string
		if err := rows.Scan(&id, &raw); err != nil {
			_ = rows.Close()
			return err
		}
		var cfg config.Server
		if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
			_ = rows.Close()
			return fmt.Errorf("decode server %d while migrating proxy credentials: %w", id, err)
		}
		changed := false
		for user, password := range cfg.Auth.Users {
			if proxyauth.IsHash(password) {
				continue
			}
			hash, err := proxyauth.Hash(password)
			if err != nil {
				_ = rows.Close()
				return fmt.Errorf("hash proxy password for server %d user %q: %w", id, user, err)
			}
			cfg.Auth.Users[user] = hash
			changed = true
		}
		if changed {
			encoded, err := json.Marshal(&cfg)
			if err != nil {
				_ = rows.Close()
				return err
			}
			updates = append(updates, update{id: id, raw: string(encoded)})
		}
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(updates) == 0 && !cleanupPending {
		return nil
	}
	// Zero deleted cells, commit all credential replacements atomically, then
	// rewrite/checkpoint the database so legacy plaintext is not left recoverable
	// in free pages or the WAL after the logical migration has completed.
	if _, err := s.db.ExecContext(ctx, `PRAGMA secure_delete=ON`); err != nil {
		return err
	}
	if len(updates) > 0 {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE security_migrations SET cleanup_pending=1 WHERE name='proxy_credentials_v1'`); err != nil {
			_ = tx.Rollback()
			return err
		}
		for _, item := range updates {
			if _, err := tx.ExecContext(ctx, `UPDATE server_configs SET config_json=? WHERE id=?`, item.raw, item.id); err != nil {
				_ = tx.Rollback()
				return err
			}
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	if _, err := s.db.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, `VACUUM`); err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE security_migrations SET cleanup_pending=0 WHERE name='proxy_credentials_v1'`); err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`)
	return err
}

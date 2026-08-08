package store

import (
	"context"
	"database/sql"
	"time"
)

type AdminUser struct {
	Username     string
	PasswordHash []byte
	CreatedAt    time.Time
}

type WebSession struct {
	TokenHash  string    `json:"id"`
	CreatedAt  time.Time `json:"created_at"`
	ExpiresAt  time.Time `json:"expires_at"`
	RemoteAddr string    `json:"remote_addr"`
	UserAgent  string    `json:"user_agent"`
}

func (s *Store) Admin(ctx context.Context) (AdminUser, error) {
	var user AdminUser
	var created string
	err := s.db.QueryRowContext(ctx, `SELECT username,password_hash,created_at FROM admin_users WHERE id=1`).Scan(&user.Username, &user.PasswordHash, &created)
	if err != nil {
		return user, err
	}
	user.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
	return user, nil
}

func (s *Store) AdminInitialized(ctx context.Context) (bool, error) {
	var exists int
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM admin_users WHERE id=1)`).Scan(&exists)
	return exists != 0, err
}

func (s *Store) CreateAdmin(ctx context.Context, username string, passwordHash []byte) error {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var exists int
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM admin_users)`).Scan(&exists); err != nil {
		return err
	}
	if exists != 0 {
		return ErrAlreadyInitialized
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := tx.ExecContext(ctx, `INSERT INTO admin_users(id,username,password_hash,created_at,updated_at) VALUES(1,?,?,?,?)`, username, passwordHash, now, now); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) UpdateAdmin(ctx context.Context, username string, passwordHash []byte) error {
	_, err := s.db.ExecContext(ctx, `UPDATE admin_users SET username=?,password_hash=?,updated_at=? WHERE id=1`,
		username, passwordHash, time.Now().UTC().Format(time.RFC3339Nano))
	return err
}

func (s *Store) CreateSession(ctx context.Context, tokenHash, remoteAddr, userAgent string, expires time.Time) error {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, err := s.db.ExecContext(ctx, `INSERT INTO web_sessions(token_hash,admin_id,created_at,expires_at,remote_addr,user_agent) VALUES(?,1,?,?,?,?)`, tokenHash, now, expires.UTC().Format(time.RFC3339Nano), remoteAddr, userAgent)
	return err
}

func (s *Store) ValidateSession(ctx context.Context, tokenHash string, now time.Time) (string, bool, error) {
	username, _, valid, err := s.ValidateSessionExpiry(ctx, tokenHash, now)
	return username, valid, err
}

// ValidateSessionExpiry validates a session and returns its absolute expiry.
// Keeping the expiry in the response lets WebUI clients enforce the same
// deadline locally instead of remaining visually unlocked until their next API
// request.
func (s *Store) ValidateSessionExpiry(ctx context.Context, tokenHash string, now time.Time) (string, time.Time, bool, error) {
	var username, expiry string
	err := s.db.QueryRowContext(ctx, `SELECT a.username,s.expires_at FROM web_sessions s JOIN admin_users a ON a.id=s.admin_id WHERE s.token_hash=?`, tokenHash).Scan(&username, &expiry)
	if err == sql.ErrNoRows {
		return "", time.Time{}, false, nil
	}
	if err != nil {
		return "", time.Time{}, false, err
	}
	expires, err := time.Parse(time.RFC3339Nano, expiry)
	if err != nil || !expires.After(now) {
		_, _ = s.db.ExecContext(context.Background(), `DELETE FROM web_sessions WHERE token_hash=?`, tokenHash)
		return "", time.Time{}, false, nil
	}
	return username, expires, true, nil
}

func (s *Store) DeleteSession(ctx context.Context, tokenHash string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM web_sessions WHERE token_hash=?`, tokenHash)
	return err
}

func (s *Store) ListSessions(ctx context.Context) ([]WebSession, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT token_hash,created_at,expires_at,remote_addr,user_agent FROM web_sessions ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var sessions []WebSession
	for rows.Next() {
		var session WebSession
		var created, expires string
		if err := rows.Scan(&session.TokenHash, &created, &expires, &session.RemoteAddr, &session.UserAgent); err != nil {
			return nil, err
		}
		session.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
		session.ExpiresAt, _ = time.Parse(time.RFC3339Nano, expires)
		sessions = append(sessions, session)
	}
	return sessions, rows.Err()
}

func (s *Store) DeleteOtherSessions(ctx context.Context, keepTokenHash string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM web_sessions WHERE token_hash<>?`, keepTokenHash)
	return err
}

func (s *Store) PruneSessions(ctx context.Context, now time.Time) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM web_sessions WHERE expires_at <= ?`, now.UTC().Format(time.RFC3339Nano))
	return err
}

// ApplySessionLifetime recalculates every existing session from its original
// login time. A changed administrator policy therefore takes effect
// immediately for already logged-in devices as well as future logins.
func (s *Store) ApplySessionLifetime(ctx context.Context, now time.Time, lifetime time.Duration) error {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT token_hash,created_at FROM web_sessions`)
	if err != nil {
		return err
	}
	type update struct {
		token  string
		expiry time.Time
		valid  bool
	}
	var updates []update
	for rows.Next() {
		var token, createdRaw string
		if err := rows.Scan(&token, &createdRaw); err != nil {
			_ = rows.Close()
			return err
		}
		created, parseErr := time.Parse(time.RFC3339Nano, createdRaw)
		expiry := created.Add(lifetime)
		updates = append(updates, update{token: token, expiry: expiry, valid: parseErr == nil && expiry.After(now)})
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, item := range updates {
		if !item.valid {
			if _, err := tx.ExecContext(ctx, `DELETE FROM web_sessions WHERE token_hash=?`, item.token); err != nil {
				return err
			}
			continue
		}
		if _, err := tx.ExecContext(ctx, `UPDATE web_sessions SET expires_at=? WHERE token_hash=?`, item.expiry.UTC().Format(time.RFC3339Nano), item.token); err != nil {
			return err
		}
	}
	return tx.Commit()
}

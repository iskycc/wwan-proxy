package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "github.com/mattn/go-sqlite3"

	"wwan-proxy/internal/config"
)

type Store struct {
	db               *sql.DB
	path             string
	bootstrapPath    string
	webListenDefault string
}

func Open(path string) (*Store, error) {
	return OpenWithWebDefault(path, "")
}

// OpenWithWebDefault injects a platform-specific first-run WebUI listener
// before any database settings are read, including redirect/relocation paths.
func OpenWithWebDefault(path, webDefault string) (*Store, error) {
	if err := validateWebDefault(webDefault); err != nil {
		return nil, err
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve database path: %w", err)
	}
	current := filepath.Clean(absolute)
	seen := make(map[string]bool)
	for hops := 0; hops < 8; hops++ {
		if seen[current] {
			return nil, fmt.Errorf("database path redirect cycle at %s", current)
		}
		seen[current] = true
		s, err := openAt(current)
		if err != nil {
			return nil, err
		}
		s.webListenDefault = webDefault
		s.bootstrapPath = filepath.Clean(absolute)
		settings, err := s.SystemSettings(context.Background())
		if err != nil {
			_ = s.Close()
			return nil, fmt.Errorf("load system settings: %w", err)
		}
		if settings.DatabasePath == "" || filepath.Clean(settings.DatabasePath) == current {
			if current != filepath.Clean(absolute) {
				bootstrap, err := openAt(filepath.Clean(absolute))
				if err != nil {
					_ = s.Close()
					return nil, fmt.Errorf("open database bootstrap pointer: %w", err)
				}
				bootstrap.webListenDefault = webDefault
				settings.DatabasePath = current
				err = bootstrap.SaveSystemSettings(context.Background(), &settings)
				_ = bootstrap.Close()
				if err != nil {
					_ = s.Close()
					return nil, fmt.Errorf("flatten database bootstrap pointer: %w", err)
				}
			}
			return s, nil
		}
		target := filepath.Clean(settings.DatabasePath)
		if !filepath.IsAbs(target) {
			_ = s.Close()
			return nil, fmt.Errorf("configured database path is not absolute: %s", target)
		}
		if _, err := os.Stat(target); os.IsNotExist(err) {
			if err := s.cloneTo(target); err != nil {
				settings.DatabasePath = current
				if saveErr := s.SaveSystemSettings(context.Background(), &settings); saveErr != nil {
					_ = s.Close()
					return nil, fmt.Errorf("relocate database to %s: %w (and restore setting: %v)", target, err, saveErr)
				}
				fmt.Fprintf(os.Stderr, "database relocation to %s failed; continuing with %s: %v\n", target, current, err)
				return s, nil
			}
		} else if err != nil {
			_ = s.Close()
			return nil, fmt.Errorf("inspect configured database path %s: %w", target, err)
		}
		_ = s.Close()
		current = target
	}
	return nil, errors.New("too many database path redirects")
}

func openAt(path string) (*Store, error) {
	if dir := filepath.Dir(path); dir != "." {
		if err := os.MkdirAll(dir, 0750); err != nil {
			return nil, fmt.Errorf("create database directory: %w", err)
		}
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, fmt.Errorf("open database file securely: %w", err)
	}
	if err := file.Close(); err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0600); err != nil {
		return nil, fmt.Errorf("restrict database permissions: %w", err)
	}
	db, err := sql.Open("sqlite3", "file:"+filepath.ToSlash(path)+"?_busy_timeout=5000&_journal_mode=WAL&_foreign_keys=on")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	s := &Store{db: db, path: path, bootstrapPath: path}
	if err := s.migrate(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := restrictSQLitePermissions(path); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

func restrictSQLitePermissions(path string) error {
	for _, candidate := range []string{path, path + "-wal", path + "-shm"} {
		if err := os.Chmod(candidate, 0600); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("restrict SQLite file %s: %w", candidate, err)
		}
	}
	return nil
}

func (s *Store) Close() error          { return s.db.Close() }
func (s *Store) Path() string          { return s.path }
func (s *Store) BootstrapPath() string { return s.bootstrapPath }

func (s *Store) cloneTo(target string) error {
	targetDir := filepath.Dir(target)
	if err := os.MkdirAll(targetDir, 0750); err != nil {
		return err
	}
	if _, err := s.db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		return err
	}
	// SQLite chooses the VACUUM INTO mode using the process umask. Stage the
	// copy inside a private directory so even an unusually permissive umask can
	// never expose credentials before we explicitly restrict the file.
	stageDir, err := os.MkdirTemp(targetDir, ".wwan-proxy-migrate-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stageDir)
	staged := filepath.Join(stageDir, filepath.Base(target))
	quoted := strings.ReplaceAll(filepath.ToSlash(staged), "'", "''")
	if _, err := s.db.Exec(`VACUUM INTO '` + quoted + `'`); err != nil {
		return err
	}
	if err := restrictSQLitePermissions(staged); err != nil {
		return err
	}
	// Link is an atomic no-replace publication on the Linux filesystems this
	// service targets. It avoids silently overwriting a destination created by
	// another process while the snapshot was being built.
	if err := os.Link(staged, target); err != nil {
		return fmt.Errorf("publish migrated database: %w", err)
	}
	return restrictSQLitePermissions(target)
}

func (s *Store) migrate(ctx context.Context) error {
	const schema = `
CREATE TABLE IF NOT EXISTS server_configs (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  name TEXT NOT NULL UNIQUE,
  enabled INTEGER NOT NULL DEFAULT 0,
  config_json TEXT NOT NULL,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS event_logs (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  timestamp TEXT NOT NULL,
  level TEXT NOT NULL,
  component TEXT NOT NULL DEFAULT '',
  server_name TEXT NOT NULL DEFAULT '',
  message TEXT NOT NULL,
  details_json TEXT NOT NULL DEFAULT '{}'
);
CREATE INDEX IF NOT EXISTS idx_event_logs_time ON event_logs(timestamp DESC);
CREATE INDEX IF NOT EXISTS idx_event_logs_level ON event_logs(level, timestamp DESC);
CREATE TABLE IF NOT EXISTS heartbeat_status (
  server_id INTEGER PRIMARY KEY REFERENCES server_configs(id) ON DELETE CASCADE,
  checked_at TEXT NOT NULL,
  healthy INTEGER NOT NULL,
  latency_ms INTEGER NOT NULL DEFAULT 0,
  status_code INTEGER NOT NULL DEFAULT 0,
  public_ip TEXT NOT NULL DEFAULT '',
  colo TEXT NOT NULL DEFAULT '',
  error TEXT NOT NULL DEFAULT '',
  trace TEXT NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS admin_users (
  id INTEGER PRIMARY KEY CHECK(id = 1),
  username TEXT NOT NULL UNIQUE,
  password_hash BLOB NOT NULL,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS web_sessions (
  token_hash TEXT PRIMARY KEY,
  admin_id INTEGER NOT NULL REFERENCES admin_users(id) ON DELETE CASCADE,
  created_at TEXT NOT NULL,
  expires_at TEXT NOT NULL,
  remote_addr TEXT NOT NULL DEFAULT '',
  user_agent TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_web_sessions_expiry ON web_sessions(expires_at);
CREATE TABLE IF NOT EXISTS system_settings (
  id INTEGER PRIMARY KEY CHECK(id = 1),
  settings_json TEXT NOT NULL,
  updated_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS security_migrations (
  name TEXT PRIMARY KEY,
  cleanup_pending INTEGER NOT NULL DEFAULT 1
);
INSERT OR IGNORE INTO security_migrations(name,cleanup_pending)
VALUES('proxy_credentials_v1',1);
CREATE TABLE IF NOT EXISTS vohive_events (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  type TEXT NOT NULL,
  device_id TEXT NOT NULL,
  server_id INTEGER,
  message TEXT NOT NULL,
  details TEXT NOT NULL DEFAULT '{}',
  created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE INDEX IF NOT EXISTS idx_vohive_events_device ON vohive_events(device_id);
CREATE INDEX IF NOT EXISTS idx_vohive_events_type ON vohive_events(type);
CREATE INDEX IF NOT EXISTS idx_vohive_events_created ON vohive_events(created_at);`
	if _, err := s.db.ExecContext(ctx, schema); err != nil {
		return fmt.Errorf("migrate sqlite: %w", err)
	}
	if err := s.migrateProxyCredentials(ctx); err != nil {
		return fmt.Errorf("migrate proxy credentials: %w", err)
	}
	return nil
}

func (s *Store) ListServers(ctx context.Context) ([]config.Server, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, enabled, config_json FROM server_configs ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []config.Server
	for rows.Next() {
		var cfg config.Server
		var id int64
		var enabled int
		var raw string
		if err := rows.Scan(&id, &enabled, &raw); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
			return nil, fmt.Errorf("decode server %d: %w", id, err)
		}
		cfg.ApplyDefaults()
		cfg.ID, cfg.Enabled = id, enabled != 0
		result = append(result, cfg)
	}
	return result, rows.Err()
}

func (s *Store) GetServer(ctx context.Context, id int64) (config.Server, error) {
	var cfg config.Server
	var enabled int
	var raw string
	err := s.db.QueryRowContext(ctx, `SELECT enabled, config_json FROM server_configs WHERE id=?`, id).Scan(&enabled, &raw)
	if err != nil {
		return cfg, err
	}
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		return cfg, err
	}
	cfg.ApplyDefaults()
	cfg.ID, cfg.Enabled = id, enabled != 0
	return cfg, nil
}

func (s *Store) SaveServer(ctx context.Context, cfg *config.Server) error {
	return s.saveServer(ctx, cfg, true)
}

// SaveServerInput persists an untrusted configuration/API representation.
// Unlike SaveServer, a hash-shaped explicit password is still treated as the
// user's literal password and hashed again. Internal snapshots use SaveServer.
func (s *Store) SaveServerInput(ctx context.Context, cfg *config.Server) error {
	return s.saveServer(ctx, cfg, false)
}

func (s *Store) saveServer(ctx context.Context, cfg *config.Server, trustStoredHashes bool) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	if err := s.prepareProxyCredentials(ctx, cfg, trustStoredHashes); err != nil {
		return err
	}
	var conflictID int64
	err := s.db.QueryRowContext(ctx, `
SELECT id FROM server_configs
WHERE id<>? AND (
  name=? OR
  json_extract(config_json,'$.listen')=? OR
  (COALESCE(json_extract(config_json,'$.http_proxy.enabled'),0)=1 AND json_extract(config_json,'$.http_proxy.listen')=?) OR
  (?=1 AND (
    json_extract(config_json,'$.listen')=? OR
    (COALESCE(json_extract(config_json,'$.http_proxy.enabled'),0)=1 AND json_extract(config_json,'$.http_proxy.listen')=?)
  ))
)
LIMIT 1`, cfg.ID, cfg.Name, cfg.Listen, cfg.Listen, boolInt(cfg.HTTPProxy.Enabled), cfg.HTTPProxy.Listen, cfg.HTTPProxy.Listen).Scan(&conflictID)
	if err == nil {
		return fmt.Errorf("server name or listen address conflicts with server %d", conflictID)
	}
	if err != sql.ErrNoRows {
		return err
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if cfg.ID == 0 {
		res, err := s.db.ExecContext(ctx, `INSERT INTO server_configs(name,enabled,config_json,created_at,updated_at) VALUES(?,?,?,?,?)`, cfg.Name, boolInt(cfg.Enabled), string(raw), now, now)
		if err != nil {
			return friendlyDBError(err)
		}
		cfg.ID, err = res.LastInsertId()
		return err
	}
	res, err := s.db.ExecContext(ctx, `UPDATE server_configs SET name=?,enabled=?,config_json=?,updated_at=? WHERE id=?`, cfg.Name, boolInt(cfg.Enabled), string(raw), now, cfg.ID)
	if err != nil {
		return friendlyDBError(err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func (s *Store) DeleteServer(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM server_configs WHERE id=?`, id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

type LogEntry struct {
	ID         int64          `json:"id"`
	Timestamp  time.Time      `json:"timestamp"`
	Level      string         `json:"level"`
	Component  string         `json:"component"`
	ServerName string         `json:"server_name"`
	Message    string         `json:"message"`
	Details    map[string]any `json:"details"`
}

func (s *Store) InsertLog(ctx context.Context, e LogEntry) error {
	if e.Timestamp.IsZero() {
		e.Timestamp = time.Now()
	}
	details, _ := json.Marshal(e.Details)
	_, err := s.db.ExecContext(ctx, `INSERT INTO event_logs(timestamp,level,component,server_name,message,details_json) VALUES(?,?,?,?,?,?)`,
		e.Timestamp.UTC().Format(time.RFC3339Nano), strings.ToUpper(e.Level), e.Component, e.ServerName, e.Message, string(details))
	return err
}

func (s *Store) ListLogs(ctx context.Context, limit int, level, query string) ([]LogEntry, error) {
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	args := make([]any, 0, 3)
	where := make([]string, 0, 2)
	if level != "" && level != "ALL" {
		where = append(where, "level=?")
		args = append(args, strings.ToUpper(level))
	}
	if query != "" {
		where = append(where, "(message LIKE ? OR server_name LIKE ? OR details_json LIKE ?)")
		q := "%" + query + "%"
		args = append(args, q, q, q)
	}
	stmt := `SELECT id,timestamp,level,component,server_name,message,details_json FROM event_logs`
	if len(where) > 0 {
		stmt += " WHERE " + strings.Join(where, " AND ")
	}
	stmt += " ORDER BY id DESC LIMIT ?"
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, stmt, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []LogEntry
	for rows.Next() {
		var e LogEntry
		var ts, details string
		if err := rows.Scan(&e.ID, &ts, &e.Level, &e.Component, &e.ServerName, &e.Message, &details); err != nil {
			return nil, err
		}
		e.Timestamp, _ = time.Parse(time.RFC3339Nano, ts)
		_ = json.Unmarshal([]byte(details), &e.Details)
		result = append(result, e)
	}
	return result, rows.Err()
}

func (s *Store) PruneLogs(ctx context.Context, before time.Time) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM event_logs WHERE timestamp < ?`, before.UTC().Format(time.RFC3339Nano))
	return err
}

type Heartbeat struct {
	ServerID   int64     `json:"server_id"`
	CheckedAt  time.Time `json:"checked_at"`
	Healthy    bool      `json:"healthy"`
	LatencyMS  int64     `json:"latency_ms"`
	StatusCode int       `json:"status_code"`
	PublicIP   string    `json:"public_ip"`
	Colo       string    `json:"colo"`
	Error      string    `json:"error"`
	Trace      string    `json:"trace,omitempty"`
}

func (s *Store) SaveHeartbeat(ctx context.Context, h Heartbeat) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO heartbeat_status(server_id,checked_at,healthy,latency_ms,status_code,public_ip,colo,error,trace)
VALUES(?,?,?,?,?,?,?,?,?) ON CONFLICT(server_id) DO UPDATE SET checked_at=excluded.checked_at,healthy=excluded.healthy,latency_ms=excluded.latency_ms,status_code=excluded.status_code,public_ip=excluded.public_ip,colo=excluded.colo,error=excluded.error,trace=excluded.trace`,
		h.ServerID, h.CheckedAt.UTC().Format(time.RFC3339Nano), boolInt(h.Healthy), h.LatencyMS, h.StatusCode, h.PublicIP, h.Colo, h.Error, h.Trace)
	return err
}

func (s *Store) ListHeartbeats(ctx context.Context) (map[int64]Heartbeat, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT server_id,checked_at,healthy,latency_ms,status_code,public_ip,colo,error,trace FROM heartbeat_status`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make(map[int64]Heartbeat)
	for rows.Next() {
		var h Heartbeat
		var ts string
		var healthy int
		if err := rows.Scan(&h.ServerID, &ts, &healthy, &h.LatencyMS, &h.StatusCode, &h.PublicIP, &h.Colo, &h.Error, &h.Trace); err != nil {
			return nil, err
		}
		h.CheckedAt, _ = time.Parse(time.RFC3339Nano, ts)
		h.Healthy = healthy != 0
		result[h.ServerID] = h
	}
	return result, rows.Err()
}

func boolInt(v bool) int {
	if v {
		return 1
	}
	return 0
}
func friendlyDBError(err error) error {
	if strings.Contains(err.Error(), "UNIQUE constraint failed") {
		return fmt.Errorf("server name or listen address already exists: %w", err)
	}
	return err
}

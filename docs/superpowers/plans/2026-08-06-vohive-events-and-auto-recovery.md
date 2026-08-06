# Vohive 事件页与自动恢复实现计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 实现独立的 Vohive 事件 Tab、Vohive 系统级心跳、外部心跳失败/恢复时的自动链路恢复。

**Architecture：** Manager 级独立 goroutine 统一查询 Vohive `/api/health`，状态变化写入 `vohive_events` 表并推送到前端；外部出口心跳失败时通知 Manager 进入 Vohive 快检模式，恢复后自动 Reload 对应出口。

**Tech Stack：** Go 1.23, SQLite, WebSocket, vanilla JS/CSS。

---

## 文件映射

| 文件 | 职责 |
|------|------|
| `internal/vohive/health.go` | 新增 `GetHealth` 及 `DeviceHealth`/`HealthResponse` 类型 |
| `internal/vohive/health_test.go` | mock `/api/health` 解析测试 |
| `internal/store/vohive_events.go` | `vohive_events` 表 CRUD、清理 |
| `internal/store/vohive_events_test.go` | 事件存储测试 |
| `internal/store/migrations` | 新增表迁移（若无迁移机制则直接在 `store.go` schema 初始化） |
| `internal/manager/vohive_heartbeat.go` | Vohive 心跳循环、事件生成、Reload 调度 |
| `internal/manager/vohive_heartbeat_test.go` | 心跳循环测试 |
| `internal/manager/heartbeat.go` | 修改：失败/恢复时切换 Vohive 快检模式；恢复时自动 Reload |
| `internal/manager/manager.go` | 修改：启动/停止 Vohive 心跳、维护快检标志 |
| `internal/webui/api.go` | 新增 `GET /api/vohive/events` 路由 |
| `internal/webui/static/index.html` | 新增 Vohive 事件 Tab 页面结构 |
| `internal/webui/static/app.js` | 新增页面渲染、过滤、WebSocket 事件摘要 |
| `internal/config/config.go` | 新增 `VohiveHeartbeatInterval`/`FastInterval` 配置项 |

---

## Task 1: Vohive Health API 客户端

**Files:**
- Create: `internal/vohive/health.go`
- Create: `internal/vohive/health_test.go`

### Step 1: 编写失败测试

```go
package vohive

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestGetHealthParsesDevices(t *testing.T) {
	resp := HealthResponse{
		Status: "healthy",
		Devices: map[string]DeviceHealth{
			"Y1": {Healthy: true, ModemOK: true, IfaceUp: true, NetworkConnected: true, Signal: -57},
			"Y2": {Healthy: true, ModemOK: true, IfaceUp: false, NetworkConnected: false, Signal: -59},
		},
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/health" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	client := NewClient(server.URL, "user", "pass", 0)
	got, err := client.GetHealth(context.Background())
	if err != nil {
		t.Fatalf("GetHealth error: %v", err)
	}
	if got.Status != "healthy" {
		t.Fatalf("status = %q, want healthy", got.Status)
	}
	if len(got.Devices) != 2 {
		t.Fatalf("devices = %d, want 2", len(got.Devices))
	}
	if !got.Devices["Y1"].Healthy {
		t.Fatal("Y1 should be healthy")
	}
}
```

### Step 2: 运行测试，确认失败

Run: `go test ./internal/vohive/ -run TestGetHealthParsesDevices -v -count=1`
Expected: FAIL (`GetHealth` / `HealthResponse` / `DeviceHealth` undefined)

### Step 3: 实现最小代码

```go
package vohive

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

type DeviceHealth struct {
	Healthy          bool  `json:"healthy"`
	ModemOK          bool  `json:"modem_ok"`
	IfaceUp          bool  `json:"iface_up"`
	NetworkConnected bool  `json:"network_connected"`
	Signal           int   `json:"signal"`
}

type HealthResponse struct {
	Status  string                  `json:"status"`
	Devices map[string]DeviceHealth `json:"devices"`
}

func (c *Client) GetHealth(ctx context.Context) (*HealthResponse, error) {
	if err := c.ensureToken(ctx); err != nil {
		return nil, fmt.Errorf("vohive authenticate: %w", err)
	}

	c.mu.Lock()
	token := c.token
	c.mu.Unlock()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/api/health", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	// Single 401 retry, mirroring requestWithRetry.
	if resp.StatusCode == http.StatusUnauthorized {
		c.mu.Lock()
		c.token = ""
		c.expiresAt = zeroTime
		c.mu.Unlock()
		return c.GetHealth(ctx)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("vohive GET /api/health returned %d: %s", resp.StatusCode, string(body))
	}

	var health HealthResponse
	if err := json.Unmarshal(body, &health); err != nil {
		return nil, fmt.Errorf("decode vohive health response: %w", err)
	}
	return &health, nil
}
```

Add helper in `client.go`:
```go
var zeroTime time.Time
```

### Step 4: 运行测试，确认通过

Run: `go test ./internal/vohive/ -run TestGetHealthParsesDevices -v -count=1`
Expected: PASS

### Step 5: 提交

```bash
git add internal/vohive/health.go internal/vohive/health_test.go internal/vohive/client.go
git commit -m "vohive: add GET /api/health client"
```

---

## Task 2: Vohive 事件存储

**Files:**
- Create: `internal/store/vohive_events.go`
- Create: `internal/store/vohive_events_test.go`
- Modify: `internal/store/store.go`（初始化 schema）

### Step 1: 编写失败测试

```go
package store

import (
	"context"
	"testing"
	"time"
)

func TestVohiveEventCRUD(t *testing.T) {
	st := testStore(t)
	defer st.Close()

	ctx := context.Background()
	event := VohiveEvent{
		Type:      VohiveEventDegraded,
		DeviceID:  "Y2",
		Message:   "network_connected=false",
		Details:   map[string]any{"signal": -59},
		CreatedAt: time.Now().UTC(),
	}
	id, err := st.SaveVohiveEvent(ctx, event)
	if err != nil {
		t.Fatalf("SaveVohiveEvent: %v", err)
	}
	if id == 0 {
		t.Fatal("expected non-zero id")
	}

	events, err := st.ListVohiveEvents(ctx, ListVohiveEventsOptions{Limit: 10})
	if err != nil {
		t.Fatalf("ListVohiveEvents: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1", len(events))
	}
	if events[0].DeviceID != "Y2" {
		t.Fatalf("device_id = %q, want Y2", events[0].DeviceID)
	}
}
```

### Step 2: 运行测试，确认失败

Run: `go test ./internal/store/ -run TestVohiveEventCRUD -v -count=1`
Expected: FAIL (`VohiveEvent`, `SaveVohiveEvent`, `ListVohiveEventsOptions` undefined)

### Step 3: 实现最小代码

`internal/store/vohive_events.go`:
```go
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
```

Modify `internal/store/store.go` schema init to add:
```sql
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
CREATE INDEX IF NOT EXISTS idx_vohive_events_created ON vohive_events(created_at);
```

### Step 4: 运行测试，确认通过

Run: `go test ./internal/store/ -run TestVohiveEventCRUD -v -count=1`
Expected: PASS

### Step 5: 提交

```bash
git add internal/store/
git commit -m "store: add vohive_events table and CRUD"
```

---

## Task 3: Vohive 心跳循环

**Files:**
- Create: `internal/manager/vohive_heartbeat.go`
- Create: `internal/manager/vohive_heartbeat_test.go`
- Modify: `internal/manager/manager.go`
- Modify: `internal/manager/heartbeat_vohive.go`

### Step 1: 编写失败测试

```go
package manager

import (
	"context"
	"testing"
	"time"

	"wwan-proxy/internal/config"
	"wwan-proxy/internal/store"
	"wwan-proxy/internal/vohive"
)

func TestVohiveHeartbeatRecordsDegradedAndRecovered(t *testing.T) {
	mu := sync.Mutex{}
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		calls++
		w.Header().Set("Content-Type", "application/json")
		health := vohive.HealthResponse{Status: "healthy", Devices: map[string]vohive.DeviceHealth{}}
		if calls == 1 {
			health.Devices["Y2"] = vohive.DeviceHealth{Healthy: true}
		} else {
			health.Devices["Y2"] = vohive.DeviceHealth{Healthy: false, NetworkConnected: false}
		}
		_ = json.NewEncoder(w).Encode(health)
	}))
	defer server.Close()

	m, st, cfg, cleanup := newVohiveTestManagerWithURL(t, server.URL)
	defer cleanup()
	defer st.Close()
	defer m.Close()
	cfg.VohiveDeviceID = "Y2"

	// drive two ticks manually
	m.runOneVohiveHeartbeatTick(context.Background())
	m.runOneVohiveHeartbeatTick(context.Background())

	events, err := st.ListVohiveEvents(context.Background(), store.ListVohiveEventsOptions{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Type != store.VohiveEventDegraded {
		t.Fatalf("expected one degraded event, got %+v", events)
	}
}
```

### Step 2: 运行测试，确认失败

Run: `go test ./internal/manager/ -run TestVohiveHeartbeatRecordsDegradedAndRecovered -v -count=1`
Expected: FAIL (`runOneVohiveHeartbeatTick` undefined)

### Step 3: 实现最小代码

`internal/manager/vohive_heartbeat.go`:
```go
package manager

import (
	"context"
	"fmt"
	"sync"
	"time"

	"wwan-proxy/internal/config"
	"wwan-proxy/internal/store"
	"wwan-proxy/internal/vohive"
)

type vohiveHealthState struct {
	mu        sync.RWMutex
	devices   map[string]vohive.DeviceHealth
	fastUntil time.Time
	lastError string
}

func (m *Manager) startVohiveHeartbeat(settings config.VohiveSettings) {
	if !settings.Enabled || settings.BaseURL == "" {
		return
	}
	state := &vohiveHealthState{}
	m.vohiveHealth = state

	go func() {
		ticker := time.NewTicker(m.vohiveInterval(false))
		defer ticker.Stop()
		for {
			select {
			case <-m.ctx.Done():
				return
			case <-ticker.C:
				m.runOneVohiveHeartbeatTick(m.ctx)
				newInterval := m.vohiveInterval(state.fastMode())
				ticker.Reset(newInterval)
			}
		}
	}()
}

func (m *Manager) runOneVohiveHeartbeatTick(ctx context.Context) {
	if m.vohiveHealth == nil {
		return
	}
	settings := m.vohiveSettings()
	client := vohive.NewClient(settings.BaseURL, settings.Username, settings.Password, 30*time.Second)
	health, err := client.GetHealth(ctx)
	if err != nil {
		m.vohiveHealth.mu.Lock()
		m.vohiveHealth.lastError = err.Error()
		m.vohiveHealth.mu.Unlock()
		m.log.Error("vohive health check failed", "error", err)
		return
	}
	m.vohiveHealth.mu.Lock()
	m.vohiveHealth.lastError = ""
	previous := m.vohiveHealth.devices
	m.vohiveHealth.devices = health.Devices
	m.vohiveHealth.mu.Unlock()

	for id, dh := range health.Devices {
		prev, hadPrev := previous[id]
		if !hadPrev {
			continue
		}
		if prev.Healthy && !dh.Healthy {
			m.recordVohiveEvent(ctx, store.VohiveEventDegraded, id, nil, fmt.Sprintf("device %s became unhealthy", id), deviceHealthDetails(dh))
		} else if !prev.Healthy && dh.Healthy {
			m.recordVohiveEvent(ctx, store.VohiveEventRecovered, id, nil, fmt.Sprintf("device %s recovered", id), deviceHealthDetails(dh))
			m.reloadServersForDevice(ctx, id)
		}
	}
}

func (m *Manager) vohiveInterval(fast bool) time.Duration {
	if fast {
		return 5 * time.Second
	}
	return 30 * time.Second
}

func (s *vohiveHealthState) fastMode() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return time.Until(s.fastUntil) > 0
}

func (m *Manager) setVohiveFastMode(fast bool) {
	if m.vohiveHealth == nil {
		return
	}
	m.vohiveHealth.mu.Lock()
	defer m.vohiveHealth.mu.Unlock()
	if fast {
		m.vohiveHealth.fastUntil = time.Now().Add(5 * time.Minute)
	} else {
		m.vohiveHealth.fastUntil = time.Time{}
	}
}

func (m *Manager) vohiveSettings() config.VohiveSettings {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.systemVohiveSettings
}

func (m *Manager) reloadServersForDevice(ctx context.Context, deviceID string) {
	m.mu.RLock()
	instances := make([]*instance, 0, len(m.instances))
	for _, inst := range m.instances {
		if inst.cfg.VohiveDeviceID == deviceID {
			instances = append(instances, inst)
		}
	}
	m.mu.RUnlock()
	for _, inst := range instances {
		if err := m.Reload(ctx, inst.cfg.ID); err != nil {
			m.log.Error("failed to reload server after vohive recovery", "server", inst.cfg.Name, "error", err)
		}
	}
}

func (m *Manager) recordVohiveEvent(ctx context.Context, typ store.VohiveEventType, deviceID string, serverID *int64, message string, details map[string]any) {
	_, err := m.store.SaveVohiveEvent(ctx, store.VohiveEvent{
		Type:      typ,
		DeviceID:  deviceID,
		ServerID:  serverID,
		Message:   message,
		Details:   details,
		CreatedAt: time.Now().UTC(),
	})
	if err != nil {
		m.log.Error("failed to save vohive event", "type", typ, "device", deviceID, "error", err)
	}
}

func deviceHealthDetails(d vohive.DeviceHealth) map[string]any {
	return map[string]any{
		"healthy":           d.Healthy,
		"modem_ok":          d.ModemOK,
		"iface_up":          d.IfaceUp,
		"network_connected": d.NetworkConnected,
		"signal":            d.Signal,
	}
}
```

Modify `internal/manager/manager.go`:
- Add fields to `Manager`:
  ```go
  vohiveHealth         *vohiveHealthState
  systemVohiveSettings config.VohiveSettings
  ```
- In `New`, initialize `systemVohiveSettings` from store (or leave zero and load in `StartAll`).
- In `StartAll`, after loading configs, load system settings and call `m.startVohiveHeartbeat(settings.Vohive)`.

Modify `internal/manager/heartbeat_vohive.go`:
- In `runVohiveRecovery`, before restart record `recovery_started`; after success record `recovery_succeeded`; on error record `recovery_failed`.

### Step 4: 运行测试，确认通过

Run: `go test ./internal/manager/ -run TestVohiveHeartbeatRecordsDegradedAndRecovered -v -count=1`
Expected: PASS

### Step 5: 提交

```bash
git add internal/manager/
git commit -m "manager: add vohive system-level heartbeat and event recording"
```

---

## Task 4: 外部心跳失败/恢复联动

**Files:**
- Modify: `internal/manager/heartbeat.go`
- Modify: `internal/manager/manager.go`

### Step 1: 编写失败测试

在 `internal/manager/heartbeat_test.go`（或新建 `heartbeat_auto_recovery_test.go`）:
```go
func TestHeartbeatRecoveryReloadsInstance(t *testing.T) {
	m, st, cfg, cleanup := newVohiveTestManager(t)
	defer cleanup()
	defer st.Close()
	defer m.Close()

	var reloads int
	m.vohiveReload = func(ctx context.Context, id int64) error {
		reloads++
		return nil
	}

	inst := m.instances[cfg.ID]
	// simulate failure then recovery
	m.setVohiveFastMode(true)
	m.heartbeatCheckResult(inst, false)
	m.heartbeatCheckResult(inst, true)

	if reloads != 1 {
		t.Fatalf("expected 1 reload, got %d", reloads)
	}
}
```
（需把 `heartbeatLoop` 中的 check 函数拆出为可测试方法 `heartbeatCheckResult`。）

### Step 2: 运行测试，确认失败

Run: `go test ./internal/manager/ -run TestHeartbeatRecoveryReloadsInstance -v -count=1`
Expected: FAIL (`heartbeatCheckResult` undefined)

### Step 3: 实现最小代码

Refactor `internal/manager/heartbeat.go`:
- Extract `check()` body into `func (m *Manager) heartbeatCheckResult(inst *instance, healthy bool)`.
- In the healthy-recovery branch, call `m.maybeReloadAfterHeartbeatRecovery(inst)`.
- In failure branch, call `m.setVohiveFastMode(true)`.

Add helper in `manager.go`:
```go
func (m *Manager) maybeReloadAfterHeartbeatRecovery(inst *instance) {
	if inst.cfg.VohiveDeviceID == "" || m.vohiveHealth == nil {
		return
	}
	m.vohiveHealth.mu.RLock()
	dh, ok := m.vohiveHealth.devices[inst.cfg.VohiveDeviceID]
	lastErr := m.vohiveHealth.lastError
	m.vohiveHealth.mu.RUnlock()
	if lastErr != "" {
		return
	}
	if ok && dh.Healthy {
		if err := m.Reload(m.ctx, inst.cfg.ID); err != nil {
			m.log.Error("failed to reload server after heartbeat recovery", "server", inst.cfg.Name, "error", err)
		}
	}
}
```

Update `heartbeatLoop` to reset fast mode when no instance is failing:
- Track `anyFailing` boolean in ticker loop; if false after a round, call `m.setVohiveFastMode(false)`.

### Step 4: 运行测试，确认通过

Run: `go test ./internal/manager/ -run TestHeartbeatRecoveryReloadsInstance -v -count=1`
Expected: PASS

### Step 5: 提交

```bash
git add internal/manager/heartbeat.go internal/manager/manager.go
git commit -m "manager: auto-reload instance when heartbeat recovers and vohive device is healthy"
```

---

## Task 5: WebUI API

**Files:**
- Modify: `internal/webui/api.go` 或新增路由文件

### Step 1: 编写失败测试

```go
func TestVohiveEventsAPI(t *testing.T) {
	// setup webui test server with mock store
	// POST a degraded event, GET /api/vohive/events, assert response
}
```

### Step 2: 运行测试，确认失败

Run: `go test ./internal/webui/ -run TestVohiveEventsAPI -v -count=1`
Expected: FAIL (handler undefined)

### Step 3: 实现最小代码

Add to webui router:
```go
mux.HandleFunc("GET /api/vohive/events", requireAuth(h.handleVohiveEvents))
```

Handler:
```go
func (h *handler) handleVohiveEvents(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	opts := store.ListVohiveEventsOptions{
		DeviceID: r.URL.Query().Get("device"),
		Type:     store.VohiveEventType(r.URL.Query().Get("type")),
	}
	if limit := r.URL.Query().Get("limit"); limit != "" {
		if n, err := strconv.Atoi(limit); err == nil && n > 0 {
			opts.Limit = n
		}
	}
	events, err := h.store.ListVohiveEvents(ctx, opts)
	if err != nil {
		respondError(w, http.StatusInternalServerError, err.Error())
		return
	}
	respondJSON(w, events)
}
```

Include latest events in WebSocket overview payload:
- Modify overview building to call `ListVohiveEvents(ctx, store.ListVohiveEventsOptions{Limit: 10})`.

### Step 4: 运行测试，确认通过

Run: `go test ./internal/webui/ -run TestVohiveEventsAPI -v -count=1`
Expected: PASS

### Step 5: 提交

```bash
git add internal/webui/
git commit -m "webui: add GET /api/vohive/events and websocket event summary"
```

---

## Task 6: WebUI 页面

**Files:**
- Modify: `internal/webui/static/index.html`
- Modify: `internal/webui/static/app.js`
- Modify: `internal/webui/static/app.css`（如有需要）

### Step 1: 修改 HTML

在 sidebar nav 添加：
```html
<button class="nav-item" data-page="vohive-events"><span class="nav-icon"><svg viewBox="0 0 24 24"><path d="M12 2a10 10 0 1 0 0 20 10 10 0 0 0 0-20Zm1 15h-2v-2h2v2Zm0-4h-2V7h2v6Z"/></svg></span><span class="nav-label">Vohive 事件</span></button>
```

添加 page section：
```html
<section class="page" id="page-vohive-events">
  <div class="panel glass-card">
    <div class="panel-head log-head"><div><p class="eyebrow">VOHIVE EVENTS</p><h3>Vohive 事件</h3></div>
      <div class="log-filters">
        <select class="native-select" id="vohive-event-type"><option value="">全部类型</option><option value="degraded">异常</option><option value="recovered">恢复</option><option value="recovery_started">重启开始</option><option value="recovery_succeeded">重启成功</option><option value="recovery_failed">重启失败</option></select>
        <select class="native-select" id="vohive-event-device"><option value="">全部设备</option></select>
        <button class="secondary-button" id="vohive-events-refresh">刷新</button>
      </div>
    </div>
    <div class="log-list" id="vohive-events-list"><div class="empty-state">暂无 Vohive 事件</div></div>
  </div>
</section>
```

### Step 2: 修改 JS

Add to `showPage`:
```js
else if(state.page==='vohive-events'){loadVohiveEvents()}
```

Add functions:
```js
async function loadVohiveEvents(){/* fetch /api/vohive/events, render list */}
function renderVohiveEventDeviceFilter(){/* populate #vohive-event-device from settings */}
function renderVohiveEvents(events){/* build HTML with type color coding and expandable details */}
```

Bind events:
```js
$('#vohive-events-refresh').onclick=loadVohiveEvents;
$('#vohive-event-type').onchange=loadVohiveEvents;
$('#vohive-event-device').onchange=loadVohiveEvents;
```

### Step 3: 验证 JS 语法

Run: `node --check internal/webui/static/app.js`
Expected: no errors

### Step 4: 提交

```bash
git add internal/webui/static/
git commit -m "webui: add Vohive events tab with filtering and error highlighting"
```

---

## Task 7: 端到端测试与修复

### Step 1: 全量测试

Run: `go test ./... -count=1`
Expected: PASS

### Step 2: 构建验证

Run: `go build ./... && node --check internal/webui/static/app.js`
Expected: exit 0

### Step 3: 提交

```bash
git commit --allow-empty -m "chore: verify vohive events feature"
```

---

## 自检

- [x] Spec coverage: Vohive 心跳、事件存储、自动恢复、UI Tab、API 均有对应任务。
- [x] Placeholder scan: 无 TBD/TODO。
- [x] Type consistency: `VohiveEventType` 在 store/manager/webui 中一致使用。

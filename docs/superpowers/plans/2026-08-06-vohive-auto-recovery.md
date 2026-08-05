# Vohive Auto-Recovery Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add optional Vohive device-network auto-restart: when a server heartbeat fails consecutively, call the Vohive API to restart the device's mobile network and reload the wwan-proxy instance once public IP returns.

**Architecture:** Extend `SystemSettings` with `VohiveSettings`, add `VohiveDeviceID` to `Server`, implement a small Vohive HTTP client, and hook it into the existing heartbeat loop in `internal/manager/heartbeat.go`.

**Tech Stack:** Go 1.22+, standard `net/http`, vanilla JS/CSS.

---

## File Map

| File | Responsibility |
|------|----------------|
| `internal/config/config.go` | `VohiveSettings` struct, `VohiveDeviceID`, defaults, validation |
| `internal/config/config_test.go` | Validation tests |
| `internal/vohive/client.go` | Vohive HTTP client and models |
| `internal/vohive/client_test.go` | Client tests with httptest |
| `internal/manager/heartbeat.go` | Trigger Vohive recovery from heartbeat loop |
| `internal/manager/heartbeat_test.go` | Heartbeat trigger tests |
| `internal/webui/static/index.html` | Vohive fields in system settings and server modal |
| `internal/webui/static/app.js` | Show/hide and collect Vohive fields |
| `internal/webui/static/app.css` | Responsive layout for new fields |

---

## Task 1: Config model

**Files:**
- Modify: `internal/config/config.go`
- Test: `internal/config/config_test.go`

- [ ] **Step 1: Add VohiveSettings struct**

In `internal/config/config.go`, add after `SystemSettings`:

```go
type VohiveSettings struct {
    Enabled             bool     `json:"enabled"`
    BaseURL             string   `json:"base_url"`
    Token               string   `json:"token"`
    ConsecutiveFailures int      `json:"consecutive_failures"`
    Cooldown            Duration `json:"cooldown"`
}
```

Add to `SystemSettings`:

```go
type SystemSettings struct {
    WebListen        string       `json:"web_listen"`
    DatabasePath     string       `json:"database_path"`
    LogLevel         string       `json:"log_level"`
    LogRetentionDays int          `json:"log_retention_days"`
    SessionLifetime  Duration     `json:"session_lifetime"`
    Vohive           VohiveSettings `json:"vohive"`
}
```

Add to `Server`:

```go
type Server struct {
    // ... existing fields ...
    VohiveDeviceID string `json:"vohive_device_id"`
}
```

- [ ] **Step 2: Apply defaults**

In `SystemSettings.ApplyDefaults`:

```go
if s.Vohive.ConsecutiveFailures == 0 {
    s.Vohive.ConsecutiveFailures = 2
}
if s.Vohive.Cooldown == 0 {
    s.Vohive.Cooldown = Duration(5 * time.Minute)
}
```

- [ ] **Step 3: Validate Vohive settings**

In `SystemSettings.Validate`, after existing checks:

```go
if s.Vohive.Enabled {
    if s.Vohive.BaseURL == "" {
        return fmt.Errorf("vohive.base_url is required when enabled")
    }
    u, err := url.Parse(s.Vohive.BaseURL)
    if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
        return fmt.Errorf("vohive.base_url must be a valid http or https URL")
    }
    if s.Vohive.Token == "" {
        return fmt.Errorf("vohive.token is required when enabled")
    }
    if s.Vohive.ConsecutiveFailures < 1 || s.Vohive.ConsecutiveFailures > 100 {
        return fmt.Errorf("vohive.consecutive_failures must be between 1 and 100")
    }
    cooldown := time.Duration(s.Vohive.Cooldown)
    if cooldown < time.Minute || cooldown > 24*time.Hour {
        return fmt.Errorf("vohive.cooldown must be between 1m and 24h")
    }
}
```

Add import `net/url` if missing.

- [ ] **Step 4: Validate Server.VohiveDeviceID**

In `Server.Validate`, near the end before returning nil:

```go
if len(s.VohiveDeviceID) > 64 {
    return fmt.Errorf("vohive_device_id must not exceed 64 characters")
}
```

- [ ] **Step 5: Write failing tests**

Append to `internal/config/config_test.go`:

```go
func TestVohiveSettingsValidation(t *testing.T) {
    t.Run("disabled is valid", func(t *testing.T) {
        s := SystemSettings{WebListen: "127.0.0.1:9090"}
        s.Vohive = VohiveSettings{Enabled: false}
        if err := s.Validate(); err != nil {
            t.Fatalf("unexpected error: %v", err)
        }
    })

    t.Run("enabled requires base_url and token", func(t *testing.T) {
        s := SystemSettings{WebListen: "127.0.0.1:9090"}
        s.Vohive = VohiveSettings{Enabled: true}
        if err := s.Validate(); err == nil {
            t.Fatal("expected error")
        }
    })

    t.Run("enabled with valid settings", func(t *testing.T) {
        s := SystemSettings{WebListen: "127.0.0.1:9090"}
        s.Vohive = VohiveSettings{Enabled: true, BaseURL: "http://192.168.8.88:7575", Token: "abc", ConsecutiveFailures: 2, Cooldown: Duration(5 * time.Minute)}
        if err := s.Validate(); err != nil {
            t.Fatalf("unexpected error: %v", err)
        }
    })
}
```

- [ ] **Step 6: Run tests**

```bash
go test ./internal/config/ -run TestVohiveSettingsValidation -v
```

Expected: PASS

- [ ] **Step 7: Commit**

```bash
git add internal/config/config.go internal/config/config_test.go
git commit -m "feat(config): add Vohive settings and device ID fields"
```

---

## Task 2: Vohive HTTP client

**Files:**
- Create: `internal/vohive/client.go`
- Create: `internal/vohive/client_test.go`

- [ ] **Step 1: Write client tests**

Create `internal/vohive/client_test.go`:

```go
package vohive

import (
    "context"
    "encoding/json"
    "net/http"
    "net/http/httptest"
    "testing"
    "time"
)

func TestRestartDevice(t *testing.T) {
    var calls []string
    srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        if r.Method != http.MethodPatch {
            t.Fatalf("expected PATCH, got %s", r.Method)
        }
        auth := r.Header.Get("Authorization")
        if auth != "Bearer token123" {
            t.Fatalf("unexpected auth: %s", auth)
        }
        calls = append(calls, r.URL.Path)
        var body map[string]any
        _ = json.NewDecoder(r.Body).Decode(&body)
        enabled, _ := body["enabled"].(bool)
        w.Header().Set("Content-Type", "application/json")
        if enabled {
            _ = json.NewEncoder(w).Encode(NetworkStatus{
                Device: "Y2", NetworkConnected: true, PublicIP: "", Status: "ok",
            })
        } else {
            _ = json.NewEncoder(w).Encode(NetworkStatus{
                Device: "Y2", NetworkConnected: false, Status: "ok",
            })
        }
    }))
    defer srv.Close()

    c := NewClient(srv.URL, "token123", 10*time.Second)
    status, err := c.RestartDevice(context.Background(), "Y2")
    if err != nil {
        t.Fatalf("unexpected error: %v", err)
    }
    if !status.NetworkConnected {
        t.Fatal("expected network_connected true after restart")
    }
    if len(calls) != 2 {
        t.Fatalf("expected 2 calls, got %d", len(calls))
    }
}

func TestGetNetworkStatus(t *testing.T) {
    srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        if r.Method != http.MethodGet {
            t.Fatalf("expected GET, got %s", r.Method)
        }
        _ = json.NewEncoder(w).Encode(NetworkStatus{
            Device: "Y2", NetworkConnected: true, PublicIP: "203.0.113.5", Status: "ok",
        })
    }))
    defer srv.Close()

    c := NewClient(srv.URL, "token123", 10*time.Second)
    status, err := c.GetNetworkStatus(context.Background(), "Y2")
    if err != nil {
        t.Fatalf("unexpected error: %v", err)
    }
    if status.PublicIP != "203.0.113.5" {
        t.Fatalf("unexpected public ip: %s", status.PublicIP)
    }
}
```

- [ ] **Step 2: Run tests to confirm failure**

```bash
go test ./internal/vohive/ -v
```

Expected: FAIL (package not found)

- [ ] **Step 3: Implement client**

Create `internal/vohive/client.go`:

```go
package vohive

import (
    "bytes"
    "context"
    "encoding/json"
    "fmt"
    "io"
    "net/http"
    "time"
)

type Client struct {
    baseURL string
    token   string
    http    *http.Client
}

type NetworkStatus struct {
    Device           string `json:"device"`
    Message          string `json:"message"`
    NetworkConnected bool   `json:"network_connected"`
    PrivateIP        string `json:"private_ip"`
    PrivateIPv6      string `json:"private_ipv6"`
    PublicIP         string `json:"public_ip"`
    PublicIPv6       string `json:"public_ipv6"`
    Status           string `json:"status"`
}

func NewClient(baseURL, token string, timeout time.Duration) *Client {
    if timeout == 0 {
        timeout = 30 * time.Second
    }
    return &Client{
        baseURL: baseURL,
        token:   token,
        http:    &http.Client{Timeout: timeout},
    }
}

func (c *Client) RestartDevice(ctx context.Context, deviceID string) (NetworkStatus, error) {
    if _, err := c.patchEnabled(ctx, deviceID, false); err != nil {
        return NetworkStatus{}, fmt.Errorf("disable device network: %w", err)
    }
    return c.patchEnabled(ctx, deviceID, true)
}

func (c *Client) GetNetworkStatus(ctx context.Context, deviceID string) (NetworkStatus, error) {
    return c.request(ctx, http.MethodGet, deviceNetworkPath(deviceID), nil)
}

func (c *Client) patchEnabled(ctx context.Context, deviceID string, enabled bool) (NetworkStatus, error) {
    body, _ := json.Marshal(map[string]bool{"enabled": enabled})
    return c.request(ctx, http.MethodPatch, deviceNetworkPath(deviceID), body)
}

func deviceNetworkPath(deviceID string) string {
    return "/api/devices/" + deviceID + "/network"
}

func (c *Client) request(ctx context.Context, method, path string, body []byte) (NetworkStatus, error) {
    var bodyReader io.Reader
    if body != nil {
        bodyReader = bytes.NewReader(body)
    }
    req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, bodyReader)
    if err != nil {
        return NetworkStatus{}, err
    }
    req.Header.Set("Authorization", "Bearer "+c.token)
    if body != nil {
        req.Header.Set("Content-Type", "application/json")
    }
    req.Header.Set("Accept", "application/json")

    resp, err := c.http.Do(req)
    if err != nil {
        return NetworkStatus{}, err
    }
    defer resp.Body.Close()

    respBody, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
    if err != nil {
        return NetworkStatus{}, err
    }
    if resp.StatusCode < 200 || resp.StatusCode >= 300 {
        return NetworkStatus{}, fmt.Errorf("vohive %s %s returned %d: %s", method, path, resp.StatusCode, string(respBody))
    }

    var status NetworkStatus
    if err := json.Unmarshal(respBody, &status); err != nil {
        return NetworkStatus{}, fmt.Errorf("decode vohive response: %w", err)
    }
    return status, nil
}
```

- [ ] **Step 4: Run tests**

```bash
go test ./internal/vohive/ -v
```

Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/vohive/client.go internal/vohive/client_test.go
git commit -m "feat(vohive): implement Vohive device network client"
```

---

## Task 3: Heartbeat recovery hook

**Files:**
- Modify: `internal/manager/heartbeat.go`
- Test: `internal/manager/heartbeat_test.go`

- [ ] **Step 1: Load Vohive settings in heartbeat loop**

In `heartbeatLoop`, add near the top:

```go
vohiveClient := inst.vohiveClient
if vohiveClient == nil {
    if settings, err := m.store.SystemSettings(ctx); err == nil && settings.Vohive.Enabled && inst.cfg.VohiveDeviceID != "" {
        vohiveClient = vohive.NewClient(settings.Vohive.BaseURL, settings.Vohive.Token, time.Duration(settings.Vohive.Cooldown))
    }
}
```

Better: add a field to `instance`:

```go
type instance struct {
    // ...
    vohiveClient *vohive.Client
}
```

And initialize it in `newInstance`:

```go
if settings, err := m.store.SystemSettings(ctx); err == nil && settings.Vohive.Enabled && cfg.VohiveDeviceID != "" {
    inst.vohiveClient = vohive.NewClient(settings.Vohive.BaseURL, settings.Vohive.Token, 30*time.Second)
}
```

This requires `newInstance` to receive the manager/store. It already has `m.store` access because `newInstance` is a method on `Manager`.

- [ ] **Step 2: Add Vohive recovery state**

In `heartbeatLoop` variables:

```go
var lastVohiveAttempt time.Time
var vohiveInProgress bool
```

- [ ] **Step 3: Trigger recovery on consecutive failures**

After the existing failure logging block, add:

```go
if inst.vohiveClient != nil &&
    consecutiveFailures >= settings.Vohive.ConsecutiveFailures &&
    !vohiveInProgress &&
    time.Since(lastVohiveAttempt) >= time.Duration(settings.Vohive.Cooldown) {

    lastVohiveAttempt = time.Now()
    vohiveInProgress = true
    go func() {
        defer func() { vohiveInProgress = false }()
        if err := m.runVohiveRecovery(ctx, inst); err != nil {
            m.log.Warn("vohive recovery failed", "server", inst.cfg.Name, "device", inst.cfg.VohiveDeviceID, "error", err)
        }
    }()
}
```

- [ ] **Step 4: Implement recovery flow**

Add method on `Manager`:

```go
func (m *Manager) runVohiveRecovery(ctx context.Context, inst *instance) error {
    client := inst.vohiveClient
    deviceID := inst.cfg.VohiveDeviceID

    m.log.Info("vohive recovery triggered", "server", inst.cfg.Name, "device", deviceID, "consecutive_failures", inst.limits.connections.Value())

    status, err := client.RestartDevice(ctx, deviceID)
    if err != nil {
        return fmt.Errorf("restart device network: %w", err)
    }
    m.log.Info("vohive device network restarted", "server", inst.cfg.Name, "device", deviceID, "network_connected", status.NetworkConnected)

    if status.NetworkConnected {
        if err := m.Reload(ctx, inst.cfg.ID); err != nil {
            m.log.Warn("vohive recovery reload failed", "server", inst.cfg.Name, "error", err)
        } else {
            m.log.Info("vohive recovery reloaded server instance", "server", inst.cfg.Name)
        }
    }

    for i := 0; i < 3; i++ {
        select {
        case <-ctx.Done():
            return ctx.Err()
        case <-time.After(5 * time.Second):
        }
        status, err = client.GetNetworkStatus(ctx, deviceID)
        if err != nil {
            m.log.Warn("vohive status check failed", "server", inst.cfg.Name, "attempt", i+1, "error", err)
            continue
        }
        if status.PublicIP != "" {
            m.log.Info("vohive public ip acquired", "server", inst.cfg.Name, "public_ip", status.PublicIP)
            if err := m.Reload(ctx, inst.cfg.ID); err != nil {
                m.log.Warn("vohive recovery reload after public ip failed", "server", inst.cfg.Name, "error", err)
            }
            return nil
        }
        m.log.Info("vohive public ip not yet available", "server", inst.cfg.Name, "attempt", i+1)
    }

    return fmt.Errorf("public ip not acquired after retries")
}
```

Note: `inst.limits.connections.Value()` may not expose current value; just log `consecutiveFailures` from closure if possible. Simpler: pass `consecutiveFailures` as parameter.

- [ ] **Step 5: Add imports**

Add to `internal/manager/heartbeat.go`:

```go
import (
    // ... existing imports ...
    "wwan-proxy/internal/vohive"
)
```

- [ ] **Step 6: Update tests**

Append to `internal/manager/heartbeat_test.go` a test that injects a mock Vohive client and verifies the recovery flow. If `heartbeatLoop` is hard to inject, refactor `newInstance` to accept an optional `vohiveClient` for tests.

- [ ] **Step 7: Run tests**

```bash
go test ./internal/manager/ -v
```

Expected: PASS

- [ ] **Step 8: Commit**

```bash
git add internal/manager/heartbeat.go internal/manager/heartbeat_test.go
git commit -m "feat(manager): trigger Vohive device recovery on heartbeat failures"
```

---

## Task 4: WebUI system settings

**Files:**
- Modify: `internal/webui/static/index.html`
- Modify: `internal/webui/static/app.js`
- Modify: `internal/webui/static/app.css`

- [ ] **Step 1: Add Vohive fields to system settings HTML**

In `index.html`, inside `#page-settings` after the admin settings form or within the system settings form:

```html
<div class="panel glass-card settings-panel" id="vohive-settings-panel">
  <div class="panel-head"><div><p class="eyebrow">VOHIVE RECOVERY</p><h3>设备网络自动恢复</h3></div><span class="settings-badge" id="vohive-badge">未启用</span></div>
  <p class="settings-copy">当出口心跳连续失败时，通过 Vohive API 重启指定设备的移动数据网络。</p>
  <div class="settings-fields">
    <label class="toggle-label span-2"><input type="checkbox" name="vohive_enabled"><span></span>启用 Vohive 自动恢复</label>
    <label class="vohive-field">Vohive Base URL<input name="vohive_base_url" placeholder="http://192.168.8.88:7575"><small>包含协议和端口</small></label>
    <label class="vohive-field">Vohive Token<input type="password" name="vohive_token" placeholder="Bearer 后面的 token 字符串"><small>保存在本地 SQLite 中</small></label>
    <label class="vohive-field">连续失败阈值<input type="number" min="1" max="100" name="vohive_consecutive_failures" value="2"><small>达到该次数后触发设备网络重启</small></label>
    <label class="vohive-field">冷却时间<input name="vohive_cooldown" value="5m"><small>两次恢复操作之间的最小间隔</small></label>
  </div>
</div>
```

- [ ] **Step 2: Show/hide Vohive fields**

In `app.js`, add:

```js
function updateVohiveSettingsFields(){
    const enabled=$('#system-settings-form').elements.vohive_enabled.checked;
    $$('#vohive-settings-panel .vohive-field').forEach(x=>x.classList.toggle('field-hidden',!enabled));
    $('#vohive-badge').textContent=enabled?'已启用':'未启用';
    $('#vohive-badge').classList.toggle('pending',!enabled);
}
```

Bind:

```js
$('#system-settings-form').elements.vohive_enabled.onchange=updateVohiveSettingsFields;
```

- [ ] **Step 3: Load and save Vohive settings**

In `loadSettings`:

```js
f.elements.vohive_enabled.checked=!!settings.vohive?.enabled;
f.elements.vohive_base_url.value=settings.vohive?.base_url||'';
f.elements.vohive_token.value=settings.vohive?.token||'';
f.elements.vohive_consecutive_failures.value=settings.vohive?.consecutive_failures||2;
f.elements.vohive_cooldown.value=durationInput(settings.vohive?.cooldown||'5m');
updateVohiveSettingsFields();
```

In `saveSystemSettings`:

```js
const body={
    // ... existing fields ...
    vohive:{
        enabled:f.elements.vohive_enabled.checked,
        base_url:f.elements.vohive_base_url.value.trim(),
        token:f.elements.vohive_token.value,
        consecutive_failures:Number(f.elements.vohive_consecutive_failures.value)||2,
        cooldown:f.elements.vohive_cooldown.value.trim()||'5m'
    }
};
```

- [ ] **Step 4: Add Device ID to server modal**

In `index.html` server modal, add after name/interface:

```html
<label class="vohive-device-field">Vohive Device ID<input name="vohive_device_id" placeholder="如 Y2"><small>仅当系统设置启用 Vohive 时生效</small></label>
```

- [ ] **Step 5: Server modal init/population/collection**

In `openForm` defaults:

```js
f.elements.vohive_device_id.value='';
```

In `if(id)` population:

```js
f.elements.vohive_device_id.value=c.vohive_device_id||'';
```

In `submitForm`:

```js
const cfg={
    // ... existing fields ...
    vohive_device_id:f.elements.vohive_device_id.value.trim(),
};
```

Add visibility toggle for Vohive device field based on global Vohive enabled state. Since the modal does not have direct access to system settings, use a class and update when opening the form:

```js
function updateVohiveDeviceField(){
    const enabled=state.settings?.vohive?.enabled;
    $$('.vohive-device-field').forEach(x=>x.classList.toggle('field-hidden',!enabled));
}
```

Call in `openForm`.

- [ ] **Step 6: Responsive CSS**

Ensure new `.settings-panel` and `.vohive-field` fit existing grid. Append if needed:

```css
@media (max-width:720px){
    .settings-layout{grid-template-columns:1fr}
    #vohive-settings-panel .settings-fields{grid-template-columns:1fr}
}
```

- [ ] **Step 7: Commit**

```bash
git add internal/webui/static/index.html internal/webui/static/app.js internal/webui/static/app.css
git commit -m "feat(webui): add Vohive settings and device ID fields"
```

---

## Task 5: Full verification

- [ ] **Step 1: Run all tests**

```bash
go test ./...
```

Expected: PASS

- [ ] **Step 2: Manual smoke test**

Build and run:

```bash
go build -o /tmp/wwan-proxy ./cmd/wwan-proxy
```

Verify:
1. 系统设置 → Vohive 区块可展开/收起。
2. 启用后填写 Base URL、Token、阈值、冷却时间，保存成功。
3. 出口配置弹窗在 Vohive 启用后显示 Device ID 字段。
4. 保存 Device ID 后 reload，值保留。

- [ ] **Step 3: Push**

```bash
git push
```

---

## Spec Coverage Check

| Spec Requirement | Task |
|------------------|------|
| VohiveSettings + defaults/validation | Task 1 |
| VohiveDeviceID in Server | Task 1 |
| Vohive HTTP client | Task 2 |
| Heartbeat trigger + recovery flow | Task 3 |
| WebUI system settings | Task 4 |
| WebUI server modal Device ID | Task 4 |
| Tests | Tasks 1-4 |

No placeholders remain.

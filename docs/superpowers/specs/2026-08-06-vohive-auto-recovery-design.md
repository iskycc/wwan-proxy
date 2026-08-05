# Vohive 设备网络自动恢复设计

## 1. 目标

当某个出口（server）的心跳连续失败达到阈值时，自动通过 Vohive API 重启该出口对应设备的移动网络，并在网络恢复后自动重载 wwan-proxy 对应实例。

## 2. 数据模型

### 2.1 系统设置新增 Vohive 全局配置

文件：`internal/config/config.go` 中的 `SystemSettings`

```go
type SystemSettings struct {
    WebListen        string       `json:"web_listen"`
    DatabasePath     string       `json:"database_path"`
    LogLevel         string       `json:"log_level"`
    LogRetentionDays int          `json:"log_retention_days"`
    SessionLifetime  Duration     `json:"session_lifetime"`
    Vohive           VohiveSettings `json:"vohive"`
}

type VohiveSettings struct {
    Enabled             bool     `json:"enabled"`
    BaseURL             string   `json:"base_url"`
    Token               string   `json:"token"`
    ConsecutiveFailures int      `json:"consecutive_failures"`
    Cooldown            Duration `json:"cooldown"`
}
```

默认值（在 `SystemSettings.ApplyDefaults` 中）：
- `Vohive.Enabled` = false
- `Vohive.ConsecutiveFailures` = 2
- `Vohive.Cooldown` = 5m

校验（在 `SystemSettings.Validate` 中）：
- 若 `Vohive.Enabled`：
  - `BaseURL` 必须为有效 http/https URL。
  - `Token` 必须非空。
  - `ConsecutiveFailures` 必须在 1..100 之间。
  - `Cooldown` 必须在 1m..24h 之间。

### 2.2 出口配置新增 Device ID

文件：`internal/config/config.go` 中的 `Server`

```go
type Server struct {
    // ... 现有字段 ...
    VohiveDeviceID string `json:"vohive_device_id"`
}
```

校验：
- 若系统 Vohive 开启且该 server 被启用：建议 `VohiveDeviceID` 非空，但不做强制要求（因为用户可能只对部分出口启用自动恢复）。
- `VohiveDeviceID` 长度不超过 64 字符。

## 3. 后端实现

### 3.1 Vohive API Client

文件：`internal/vohive/client.go`

类型：

```go
type Client struct {
    baseURL string
    token   string
    http    *http.Client
}

func NewClient(baseURL, token string, timeout time.Duration) *Client

func (c *Client) RestartDevice(ctx context.Context, deviceID string) (NetworkStatus, error)
func (c *Client) GetNetworkStatus(ctx context.Context, deviceID string) (NetworkStatus, error)

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
```

`RestartDevice` 流程：
1. PATCH `/api/devices/{deviceID}/network` with `{"enabled": false}`
2. 等待返回。
3. PATCH `/api/devices/{deviceID}/network` with `{"enabled": true}`
4. 等待并解析返回为 `NetworkStatus`。

`GetNetworkStatus` 流程：
- GET `/api/devices/{deviceID}/network`
- 解析返回。

注意：用户示例中两次都是 PATCH，但第二次返回状态。5 秒后的检查可以使用 GET（如果 Vohive 支持），也可以再次 PATCH `enabled=true` 刷新状态。为安全起见，先尝试 GET；若返回 404 则回退到 PATCH `enabled=true`。

### 3.2 集成到心跳循环

文件：`internal/manager/heartbeat.go`

修改 `heartbeatLoop`：

1. 新增状态字段：
   - `consecutiveFailures int`（已存在）
   - `lastVohiveAttempt time.Time`
   - `vohiveInProgress bool`

2. 当心跳失败时，原有逻辑不变，继续累加 `consecutiveFailures`。

3. 新增 Vohive 触发检查：
   - 当 `consecutiveFailures >= vohive.ConsecutiveFailures` 时。
   - 当 `vohive.Enabled` 为 true。
   - 当 `inst.cfg.VohiveDeviceID` 非空。
   - 当 `time.Since(lastVohiveAttempt) >= cooldown`。
   - 当 `!vohiveInProgress`。

4. 触发流程（在独立 goroutine 中，避免阻塞心跳 ticker）：
   - 设置 `vohiveInProgress = true`。
   - 记录 `lastVohiveAttempt = time.Now()`。
   - 调用 `client.RestartDevice`。
   - 若返回 `NetworkConnected == true`：
     - 立即调用 `m.manager.Reload(ctx, inst.cfg.ID)` 重载实例。
   - 等待 5 秒。
   - 调用 `client.GetNetworkStatus`（或 PATCH enabled=true），检查 `PublicIP` 是否非空。
   - 若为空，重试最多 3 次，每次间隔 5 秒。
   - 若最终 `PublicIP` 非空，再次 `m.manager.Reload(ctx, inst.cfg.ID)`（可选，若第一次 reload 后心跳仍未恢复）。
   - 设置 `vohiveInProgress = false`。

5. 心跳恢复时（previousHealthy 从 false 变为 true）：
   - 重置 `consecutiveFailures = 0`。

### 3.3 Manager 依赖注入

`Manager` 需要访问系统设置中的 Vohive 配置。当前 `Manager` 只持有 `store`，可以在心跳循环中通过 `m.store.SystemSettings(ctx)` 获取。

为避免每次心跳都查 SQLite，可以：
- 在 `Manager` 中缓存 `VohiveSettings`。
- 在系统设置保存时通知 `Manager` 刷新缓存。

为简化，本次先在心跳循环中读取；若性能问题后续优化。

### 3.4 错误处理与日志

- Vohive 调用失败只记录 WARN，不阻断心跳循环。
- 记录关键事件：触发 Vohive 重启、重启成功、public_ip 获取成功/失败、实例 reload。

## 4. WebUI 改造

### 4.1 系统设置页

新增“Vohive 设备管理”折叠/区块：
- 启用开关 `vohive_enabled`
- Base URL `vohive_base_url`
- Token `vohive_token`
- 连续失败阈值 `vohive_consecutive_failures`
- 冷却时间 `vohive_cooldown`

交互：
- 未启用时，其他字段隐藏。
- Token 输入框为 `type="password"`。

### 4.2 出口配置弹窗

- 新增字段“Vohive Device ID”。
- 仅当系统设置中 Vohive 启用时才显示。

### 4.3 响应式

- 新字段加入现有 `.form-grid`，自动继承响应式规则。
- 系统设置页两列布局在窄屏下改为单列。

## 5. 测试

- `internal/vohive/client_test.go`：
  - 测试 RestartDevice 两次 PATCH 顺序正确。
  - 测试 GetNetworkStatus 解析。
  - 测试错误返回处理。
- `internal/config/config_test.go`：
  - 测试 VohiveSettings 校验。
  - 测试 Server.VohiveDeviceID 长度校验。
- `internal/manager/heartbeat_test.go`：
  - 测试连续失败触发 Vohive 重启（注入 mock client）。

## 6. 影响文件

- `internal/config/config.go`
- `internal/config/config_test.go`
- `internal/vohive/client.go`（新增）
- `internal/vohive/client_test.go`（新增）
- `internal/manager/heartbeat.go`
- `internal/manager/heartbeat_test.go`
- `internal/webui/static/index.html`
- `internal/webui/static/app.js`
- `internal/webui/static/app.css`

## 7. 后续扩展

- Vohive client 支持 TLS 跳过校验。
- 支持多个 Vohive 设备按优先级 fallback。
- 支持 Vohive 重启后自动切换实例启用/禁用状态。

# Vohive 事件页与自动恢复设计

## 目标
为 WebUI 新增独立的 **Vohive 事件** Tab，集中展示 Vohive API 健康检查、设备网络异常、自动重启及恢复事件；同时引入 Vohive 系统级心跳，并在外部心跳失败/恢复时自动调整检测频率并恢复出口链路。

## 架构
采用 **Manager 级独立 Vohive 心跳循环**（方案 B）：
- 一个 goroutine 统一调用 `GET /api/health`，结果供所有绑定 Vohive Device ID 的出口共享。
- 心跳状态变化时写入 `vohive_events` 表，并通过 WebSocket/API 推送到前端。
- 外部出口心跳（1.1.1.1）失败时加快 Vohive 检测频率；恢复时自动 Reload 对应出口实例。

## 组件

### 1. Vohive 健康客户端
- 文件：`internal/vohive/health.go`
- 提供 `Client.GetHealth(ctx) (*HealthResponse, error)`，解析 `/api/health` 返回的 devices 映射。

### 2. Vohive 心跳循环
- 文件：`internal/manager/vohive_heartbeat.go`
- 在 `Manager.StartAll` 后启动（若 Vohive 启用）。
- 正常间隔 30s；任一外部心跳失败时切换为 5s；所有外部心跳恢复 30s 后恢复常规定时间隔。
- 比较本次与上次 health 结果，生成事件：
  - `degraded`：device 由 healthy 变为 unhealthy。
  - `recovered`：device 由 unhealthy 变为 healthy。
- 当 Vohive 恢复触发时，对绑定该 device 的所有出口执行 `Reload`。

### 3. 事件存储
- 文件：`internal/store/vohive_events.go`
- 新增表 `vohive_events`：
  ```sql
  CREATE TABLE vohive_events (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    type TEXT NOT NULL,
    device_id TEXT NOT NULL,
    server_id INTEGER,
    message TEXT NOT NULL,
    details TEXT NOT NULL DEFAULT '{}',
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
  );
  ```
- 提供 `SaveVohiveEvent`、`ListVohiveEvents`、`PruneVohiveEvents`。

### 4. WebUI API
- 文件：`internal/webui/`（在现有路由中新增）
- `GET /api/vohive/events?device=&type=&limit=` 返回事件列表。
- WebSocket overview 消息中附带最近 10 条 Vohive 事件摘要。

### 5. WebUI 页面
- 文件：`internal/webui/static/index.html`、`internal/webui/static/app.js`
- 侧边栏新增 “Vohive 事件” Tab。
- 页面展示事件列表：时间、设备、类型、消息、详情（signal、healthy、network_connected 等）。
- 支持按 device/type 过滤；错误/失败事件醒目显示。

### 6. 外部心跳自动恢复
- 文件：`internal/manager/heartbeat.go`
- 在 `h.Healthy` 由 false 转为 true 时：
  - 清除心跳错误；
  - 若该出口配置了 `vohive_device_id` 且 Vohive 中该 device 当前健康，则调用 `Reload(id)` 恢复链路。
- 加快 Vohive 检测频率：通过 `Manager` 上的原子标志 `vohiveFastMode`，由 `heartbeatLoop` 在失败时置位，恢复时根据全局状态清除。

## 数据模型

```go
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
```

## 事件记录规则

| 场景 | 事件类型 | 说明 |
|------|---------|------|
| /api/health 中 device 由 healthy→false | degraded | 记录时间及 health 快照 |
| /api/health 中 device 由 false→healthy | recovered | 记录时间及 health 快照，并 Reload 对应出口 |
| 触发 Vohive 设备网络重启 | recovery_started | 关联 server_id |
| 重启后 public_ip 确认 | recovery_succeeded | 关联 server_id |
| 重启失败或超时 | recovery_failed | 关联 server_id，错误详情入库 |

## 错误处理与 UI 反馈
- Vohive API 调用失败（登录失败、网络不可达、返回非 200）产生 `degraded`/`recovery_failed` 事件，message 包含具体错误原因。
- 前端事件列表中 ERROR 类型使用红色高亮，点击可展开 details JSON。
- 若 Vohive 心跳循环本身连续失败 3 次，在 overview 顶部显示持久警告横幅。

## 测试
- `internal/vohive/health_test.go`：mock `/api/health` 解析。
- `internal/store/vohive_events_test.go`：事件 CRUD 与清理。
- `internal/manager/vohive_heartbeat_test.go`：状态转换、事件生成、Reload 触发、快慢模式切换。
- `internal/webui/`：API 列表与 WebSocket 推送测试。

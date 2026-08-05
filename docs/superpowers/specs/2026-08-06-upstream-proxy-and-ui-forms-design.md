# 设计方案：出口级上游 SOCKS5 代理 + UI 表单优化

## 1. 目标

1. 为每个代理出口（`config.Server`）增加可选的上游 SOCKS5 代理支持，支持无认证和用户名/密码认证，用于链式代理。
2. 把 WebUI 中两处手写 JSON 的输入（`auth_users`、`udp_map`）改造成结构化表单。
3. 在改造弹窗时顺带加固响应式布局，缓解小屏幕下的字体/挤压变形问题。

## 2. 数据模型

### 2.1 新增 `Upstream` 配置

文件：`internal/config/config.go`

```go
type Upstream struct {
    Enabled    bool   `json:"enabled"`
    Address    string `json:"address"`      // host:port 或 ip:port
    AuthMethod string `json:"auth_method"`  // "none" | "username_password"
    Username   string `json:"username"`
    Password   string `json:"password"`
}
```

在 `Server` 中新增字段：

```go
type Server struct {
    // ... existing fields ...
    Upstream Upstream `json:"upstream"`
}
```

### 2.2 默认值与校验

在 `Server.ApplyDefaults` 中：
- `Upstream.AuthMethod` 为空时设为 `"none"`。

在 `Server.Validate` 中：
- 若 `Upstream.Enabled`：
  - `Address` 必须非空且格式为 `host:port`。
  - `AuthMethod` 必须是 `"none"` 或 `"username_password"`。
  - 若 `AuthMethod == "username_password"`：`Username` 和 `Password` 必须非空且各自长度 1..255 字节。
- `Upstream` 与 `Interface` 不冲突：上游连接仍然通过本出口网口发出（`bindToDevice`）。

### 2.3 凭证处理

- 前端提交真实密码。
- 后端保存时，如果 `AuthMethod == "username_password"`，密码明文保存于 SQLite（上游代理凭据由管理员提供，与本地认证一致处理方式）。
- 后端返回前端时脱敏（redact）：`Password` 返回空字符串，前端通过 `password_unchanged` 风格或 simply 始终提交完整表单来避免误清空。
- 为简化实现，上游密码在编辑弹窗中**每次重新显示为空**，保存时必须重新填写。UI 通过小字提示“留空表示不修改”来避免误覆盖——本次先不做复杂的“密码未变更”标记，若后续需要再补充。

## 3. 后端实现

### 3.1 新增 SOCKS5 上游客户端

文件：`internal/socks5/upstream.go`

提供函数：

```go
// DialViaUpstream 通过上游 SOCKS5 代理建立到目标地址的 TCP 连接。
// 它返回的 net.Conn 已经在上游代理侧完成 CONNECT 握手，可直接读写目标数据。
func DialViaUpstream(
    ctx context.Context,
    upstream config.Upstream,
    dialer *net.Dialer,           // 已绑定出口网卡的 dialer
    network, targetAddress string, // "tcp" / "tcp4" / "tcp6" 与 "host:port"
) (net.Conn, error)
```

实现步骤：
1. 用 `dialer.DialContext(ctx, "tcp", upstream.Address)` 连接上游 SOCKS5。
2. 发送 greeting：`[0x05, 0x01, 0x00]`（无认证）或 `[0x05, 0x01, 0x02]`（用户名密码）。
3. 读取服务器选择的认证方法：
   - `0x00`：无认证。
   - `0x02`：发送 username/password 子协商 `[0x01, ulen, username..., plen, password...]`，读取状态 `0x00` 表示成功。
   - `0xFF` 或其他：返回错误。
4. 发送 CONNECT 请求：`[0x05, 0x01, 0x00, atyp, ...addr..., port]`。
   - atyp：IPv4 `0x01`、域名 `0x03`、IPv6 `0x04`。
   - 目标地址来自 `targetAddress` 的 host；端口来自 `targetAddress` 的 port。
5. 读取 reply，REP `0x00` 表示成功；否则返回对应错误。
6. 返回连接。

错误映射：
- REP `0x01` → 一般错误
- REP `0x02` → 规则不允许
- REP `0x03` → 网络不可达
- REP `0x04` → 主机不可达
- REP `0x05` → 连接拒绝
- REP `0x06` → TTL 过期
- REP `0x07` → 命令不支持
- REP `0x08` → 地址类型不支持

### 3.2 集成到 `Server.dialContext`

文件：`internal/socks5/server.go`

在 `dialContext` 中，构造 `dialer` 后：

```go
dialer := s.dialerWithoutResolver() // 已绑定 device
if s.cfg.Upstream.Enabled {
    return DialViaUpstream(ctx, s.cfg.Upstream, dialer, network, address)
}
// ... existing connect logic ...
```

注意：
- 上游连接本身也要经过 `bindToDevice`，确保从本出口网口出去。
- 若目标地址是域名，上游 SOCKS5 协议支持直接发送域名（atyp=0x03），因此本地无需先解析目标域名，直接把域名交给上游。这也能避免 ACL/DNS 解析与上游的耦合。
- 若本地配置了 `IPv4Only`，仍然可以在 CONNECT 请求里发送域名；上游决定解析行为。若需要强制 IPv4，可以在上游握手前本地解析再传 IP——本次先保持简单，直接传域名。

### 3.3 HTTP Proxy 自动支持

`internal/httpproxy/server.go` 使用 `srv.DialContext` 作为出站 dialer，因此无需额外改动即可支持上游 SOCKS5。

### 3.4 BIND 与 UDP ASSOCIATE 的处理

- **BIND**：逻辑较复杂。上游 SOCKS5 对 BIND 的语义是“让上游监听并返回二次连接地址”。本次**不支持**上游代理下的 BIND；若 `Upstream.Enabled` 且收到 BIND 命令，直接回复 `repCommandNotSupported` 或 `repNotAllowed`，并在日志中说明。后续需要可再扩展。
- **UDP ASSOCIATE**：标准 SOCKS5 的 UDP ASSOCIATE 需要在 TCP 控制连接上发送 UDP ASSOCIATE 请求，上游返回 UDP relay 地址，然后本地通过 UDP 与该 relay 通信。实现复杂度较高，本次**暂不支持**。若 `Upstream.Enabled` 且收到 UDP ASSOCIATE，回复 `repCommandNotSupported` 并记录日志。
- 文档与 UI 中明确提示：启用上游后 BIND 和 UDP ASSOCIATE 不可用。

### 3.5 心跳探测

`ProbeDialContext` 也走 `dialContext` 路径，因此心跳也会经过上游代理。这是预期行为：探测验证的是“经过上游后能否访问心跳 URL”。

## 4. WebUI 改造

### 4.1 上游代理表单区块

在出口配置弹窗中新增折叠/可切换区块“上游代理（链式）”，位于“基础设置”之后或“访问认证”之前。

字段：
- 启用开关 `upstream_enabled`
- 上游地址 `upstream_address`（placeholder: `上级 SOCKS5 地址，如 10.0.0.1:1080`）
- 认证方式 `upstream_auth_method`（none / username_password）
- 用户名 `upstream_username`
- 密码 `upstream_password`（type=password）

交互：
- 未启用时，地址/认证字段隐藏。
- 认证方式为 none 时，用户名密码隐藏。
- 保存时若密码为空且后端返回的密码为空，视为“未修改”——但本次简化处理：编辑时密码框为空，保存成功后清空；如果用户没填密码就保存且启用了用户名密码认证，后端校验失败并提示。

### 4.2 认证用户表格化

把当前的：

```html
<label class="auth-users-field">用户 JSON<input name="auth_users" placeholder='{"user":"password"}'>
```

替换为动态表格：

```html
<div class="auth-users-field">
  <label>本地代理用户</label>
  <div id="auth-users-list"></div>
  <button type="button" id="auth-users-add">＋ 添加用户</button>
  <small>编辑时留空密码表示保持原密码；新增用户必须填写密码。</small>
</div>
```

JS 逻辑：
- `openForm` 时从 `c.auth.users` 渲染行，每行 username + password + 删除按钮。
- 密码输入 placeholder/提示：新增时必填，编辑已有用户时留空表示不修改。
- `submitForm` 时收集所有行，构建 `{user: password}` 对象，同时生成 `password_unchanged` 数组（密码为空的已有用户）。
- 后端 `redactServerCredential` 已经会把密码替换为空并填充 `password_unchanged`，前端需要读取 `password_unchanged` 来识别哪些用户是编辑态、密码可留空。

### 4.3 UDP 广播映射表格化

把当前的：

```html
<label class="span-2">广播地址映射 JSON<input name="udp_map" placeholder='{"192.168.1.1":"203.0.113.10"}'>
```

替换为动态表格：

```html
<div class="span-2">
  <label>UDP 广播地址映射</label>
  <div id="udp-map-list"></div>
  <button type="button" id="udp-map-add">＋ 添加映射</button>
  <small>当 UDP 监听地址为内网 IP 时，可指定对外广播的公网 IP。</small>
</div>
```

JS 逻辑：
- 每行两个输入：本地 IP、Relay IP，加删除按钮。
- `openForm` 时从 `c.udp.advertise_map` 渲染。
- `submitForm` 时收集为对象。

### 4.4 响应式/字体加固

- 在 `app.css` 中为 `.modal` 增加 `max-height: 92vh; overflow-y: auto;` 并检查现有是否已有滚动。
- 为小屏幕增加 media query：`.form-grid` 在宽度小于 720px 时改为单列（`grid-template-columns: 1fr`），避免字段挤压。
- 检查固定 `font-size` 是否过小，必要时把表单标签 `small` 提示文字从当前可能偏小的尺寸调大到至少 12px。
- 具体数值以实际 CSS 为准，避免大范围重构。

## 5. 测试计划

### 5.1 单元测试

- `internal/socks5/upstream_test.go`：
  - 启动一个内存中的 SOCKS5 测试服务器（复用 `Server` 或写最小 mock）。
  - 测试无认证上游 CONNECT 成功。
  - 测试用户名/密码认证上游 CONNECT 成功。
  - 测试上游返回各种 REP 时的错误映射。
- `internal/config/config_test.go`：
  - 新增 `Upstream` 校验通过/失败用例。
- `internal/webui/server_test.go` / `settings_test.go`：
  - 若已有保存 server 的测试，补充 `upstream` 字段的 round-trip。

### 5.2 集成/手动测试

- 配置一个出口指向本地第二个 `wwan-proxy` 实例作为上游，验证 curl 能否通过链式代理访问目标。
- 验证启用上游后 BIND 和 UDP ASSOCIATE 被正确拒绝。
- 验证 WebUI 中 auth_users / udp_map 表格添加、删除、保存、回显正确。

## 6. 依赖与影响面

- 不引入新外部依赖，纯标准库实现 SOCKS5 客户端。
- 影响文件：
  - `internal/config/config.go`
  - `internal/socks5/server.go`
  - `internal/socks5/upstream.go`（新增）
  - `internal/socks5/upstream_test.go`（新增）
  - `internal/webui/static/index.html`
  - `internal/webui/static/app.js`
  - `internal/webui/static/app.css`
  - 相关测试文件

## 7. 后续可扩展

- 上游 SOCKS5 支持 BIND / UDP ASSOCIATE。
- 上游支持 TLS（SOCKS5 over TLS）。
- 上游支持 Happy Eyeball / 多地址轮询。
- 上游健康探测与 fallback。

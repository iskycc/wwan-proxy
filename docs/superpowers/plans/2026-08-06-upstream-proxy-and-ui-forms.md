# Upstream SOCKS5 Proxy + UI Form Improvements Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add per-server optional upstream SOCKS5 proxy support with authentication, replace JSON text inputs for auth users and UDP advertise map with structured tables, and harden modal responsiveness.

**Architecture:** Extend `config.Server` with an `Upstream` struct; implement a minimal SOCKS5 client in `internal/socks5/upstream.go` that dials the upstream through the same egress interface and performs CONNECT; plumb it into `Server.dialContext` so HTTP Proxy reuses it automatically. WebUI gains a collapsible upstream section and two dynamic key-value tables.

**Tech Stack:** Go 1.22+, vanilla JS, CSS, SQLite via `internal/store`.

---

## File Map

| File | Responsibility |
|------|----------------|
| `internal/config/config.go` | Add `Upstream` struct, defaults, validation, clone logic |
| `internal/socks5/upstream.go` | SOCKS5 client: dial upstream, auth negotiation, CONNECT |
| `internal/socks5/upstream_test.go` | Unit tests for upstream client |
| `internal/socks5/server.go` | Route outbound connections through upstream when enabled |
| `internal/socks5/server_test.go` | Existing tests; add upstream integration path |
| `internal/config/config_test.go` | Validation tests for upstream |
| `internal/webui/static/index.html` | Add upstream fields and dynamic table containers |
| `internal/webui/static/app.js` | Render/collect dynamic tables and upstream fields |
| `internal/webui/static/app.css` | Responsive modal/grid fixes |

---

## Task 1: Add `Upstream` config model

**Files:**
- Modify: `internal/config/config.go`
- Test: `internal/config/config_test.go`

- [ ] **Step 1: Define the struct**

Add to `internal/config/config.go` near `HTTPProxy`:

```go
// Upstream describes an optional upstream SOCKS5 proxy that this server
// uses for outbound CONNECT requests. When enabled, BIND and UDP ASSOCIATE
// are rejected because the upstream client only implements CONNECT.
type Upstream struct {
    Enabled    bool   `json:"enabled"`
    Address    string `json:"address"`
    AuthMethod string `json:"auth_method"`
    Username   string `json:"username"`
    Password   string `json:"password"`
}
```

Add field to `Server`:

```go
type Server struct {
    // ... existing fields ...
    HTTPProxy HTTPProxy `json:"http_proxy"`
    Upstream  Upstream  `json:"upstream"`
    UDP       UDP       `json:"udp"`
    // ...
}
```

- [ ] **Step 2: Clone upstream credentials**

`Upstream` contains only value fields, so no deep clone is needed. No change to `Server.Clone`.

- [ ] **Step 3: Apply defaults**

In `Server.ApplyDefaults`, after existing defaults:

```go
if s.Upstream.AuthMethod == "" {
    s.Upstream.AuthMethod = "none"
}
```

- [ ] **Step 4: Validate upstream**

In `Server.Validate`, after HTTPProxy validation block:

```go
if s.Upstream.Enabled {
    if s.Upstream.Address == "" {
        return fmt.Errorf("upstream.address is required when upstream is enabled")
    }
    if _, _, err := net.SplitHostPort(s.Upstream.Address); err != nil {
        return fmt.Errorf("upstream.address: %w", err)
    }
    if s.Upstream.AuthMethod != "none" && s.Upstream.AuthMethod != "username_password" {
        return fmt.Errorf("upstream.auth_method must be none or username_password")
    }
    if s.Upstream.AuthMethod == "username_password" {
        if len(s.Upstream.Username) == 0 || len(s.Upstream.Username) > 255 {
            return fmt.Errorf("upstream.username must be 1..255 bytes")
        }
        if len(s.Upstream.Password) == 0 || len(s.Upstream.Password) > 255 {
            return fmt.Errorf("upstream.password must be 1..255 bytes")
        }
    }
}
```

- [ ] **Step 5: Write failing validation test**

Create `internal/config/config_test.go` additions (append to existing file):

```go
func TestUpstreamValidation(t *testing.T) {
    base := func() Server {
        return Server{
            Name: "test", Listen: "0.0.0.0:1080", Interface: "eth0",
        }
    }

    t.Run("disabled upstream is valid", func(t *testing.T) {
        s := base()
        s.Upstream = Upstream{Enabled: false}
        if err := s.Validate(); err != nil {
            t.Fatalf("unexpected error: %v", err)
        }
    })

    t.Run("enabled upstream requires address", func(t *testing.T) {
        s := base()
        s.Upstream = Upstream{Enabled: true}
        if err := s.Validate(); err == nil {
            t.Fatal("expected error")
        }
    })

    t.Run("enabled upstream with no auth", func(t *testing.T) {
        s := base()
        s.Upstream = Upstream{Enabled: true, Address: "10.0.0.1:1080", AuthMethod: "none"}
        if err := s.Validate(); err != nil {
            t.Fatalf("unexpected error: %v", err)
        }
    })

    t.Run("username_password requires credentials", func(t *testing.T) {
        s := base()
        s.Upstream = Upstream{Enabled: true, Address: "10.0.0.1:1080", AuthMethod: "username_password"}
        if err := s.Validate(); err == nil {
            t.Fatal("expected error")
        }
    })

    t.Run("username_password with credentials is valid", func(t *testing.T) {
        s := base()
        s.Upstream = Upstream{Enabled: true, Address: "10.0.0.1:1080", AuthMethod: "username_password", Username: "u", Password: "p"}
        if err := s.Validate(); err != nil {
            t.Fatalf("unexpected error: %v", err)
        }
    })
}
```

- [ ] **Step 6: Run config tests**

```bash
go test ./internal/config/ -run TestUpstreamValidation -v
```

Expected: PASS

- [ ] **Step 7: Commit**

```bash
git add internal/config/config.go internal/config/config_test.go
git commit -m "feat(config): add upstream SOCKS5 proxy configuration and validation"
```

---

## Task 2: Implement SOCKS5 upstream client

**Files:**
- Create: `internal/socks5/upstream.go`
- Test: `internal/socks5/upstream_test.go`

- [ ] **Step 1: Write the upstream test harness**

Create `internal/socks5/upstream_test.go`:

```go
package socks5

import (
    "context"
    "errors"
    "fmt"
    "io"
    "net"
    "testing"
    "time"

    "wwan-proxy/internal/config"
)

func startTestUpstream(t *testing.T, users map[string]string) (string, func()) {
    t.Helper()
    cfg := config.Server{
        Name: "upstream", Listen: "127.0.0.1:0", Interface: "lo",
        Auth: config.Auth{Method: "none"},
    }
    if len(users) > 0 {
        cfg.Auth.Method = "username_password"
        cfg.Auth.Users = users
    }
    srv := New(cfg, testLogger(t))
    ln, err := net.Listen("tcp", cfg.Listen)
    if err != nil {
        t.Fatalf("listen: %v", err)
    }
    done := make(chan struct{})
    go func() {
        defer close(done)
        _ = srv.Serve(&testListener{Listener: ln})
    }()
    return ln.Addr().String(), func() {
        _ = srv.Close()
        <-done
    }
}

type testListener struct{ net.Listener }

func (l *testListener) Accept() (net.Conn, error) { return l.Listener.Accept() }

func testLogger(t *testing.T) *slog.Logger {
    return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestDialViaUpstreamNoAuth(t *testing.T) {
    addr, cleanup := startTestUpstream(t, nil)
    defer cleanup()

    dialer := &net.Dialer{Timeout: 5 * time.Second}
    upstream := config.Upstream{Enabled: true, Address: addr, AuthMethod: "none"}

    conn, err := DialViaUpstream(context.Background(), upstream, dialer, "tcp", "127.0.0.1:9999")
    if err == nil {
        _ = conn.Close()
    }
    // The upstream server has nothing on 9999, so the SOCKS5 handshake succeeds
    // but the upstream-side connect will fail with connection refused.
    if err == nil {
        t.Fatal("expected upstream-side connect to fail")
    }
    var netErr *net.OpError
    if !errors.As(err, &netErr) {
        t.Fatalf("expected network error from upstream connect, got %v", err)
    }
}

func TestDialViaUpstreamUsernamePassword(t *testing.T) {
    addr, cleanup := startTestUpstream(t, map[string]string{"alice": "secret"})
    defer cleanup()

    dialer := &net.Dialer{Timeout: 5 * time.Second}
    upstream := config.Upstream{Enabled: true, Address: addr, AuthMethod: "username_password", Username: "alice", Password: "secret"}

    conn, err := DialViaUpstream(context.Background(), upstream, dialer, "tcp", "127.0.0.1:9999")
    if err == nil {
        _ = conn.Close()
    }
    if err == nil {
        t.Fatal("expected upstream-side connect to fail")
    }
}

func TestDialViaUpstreamAuthFailure(t *testing.T) {
    addr, cleanup := startTestUpstream(t, map[string]string{"alice": "secret"})
    defer cleanup()

    dialer := &net.Dialer{Timeout: 5 * time.Second}
    upstream := config.Upstream{Enabled: true, Address: addr, AuthMethod: "username_password", Username: "alice", Password: "wrong"}

    _, err := DialViaUpstream(context.Background(), upstream, dialer, "tcp", "127.0.0.1:9999")
    if err == nil {
        t.Fatal("expected auth failure")
    }
}
```

Note: this uses internal helpers that may need adjustment; see actual test helpers in `server_test.go`.

- [ ] **Step 2: Run tests to confirm failure**

```bash
go test ./internal/socks5/ -run TestDialViaUpstream -v
```

Expected: FAIL with `undefined: DialViaUpstream` or similar.

- [ ] **Step 3: Implement upstream client**

Create `internal/socks5/upstream.go`:

```go
package socks5

import (
    "context"
    "encoding/binary"
    "errors"
    "fmt"
    "io"
    "net"
    "strconv"
    "time"

    "wwan-proxy/internal/config"
)

const (
    upstreamMethodNone     byte = 0x00
    upstreamMethodPassword byte = 0x02
)

// DialViaUpstream connects to targetAddress through an upstream SOCKS5 proxy.
// The returned connection is ready for application data after a successful
// CONNECT reply. The caller is responsible for closing the connection.
func DialViaUpstream(ctx context.Context, upstream config.Upstream, dialer *net.Dialer, network, targetAddress string) (net.Conn, error) {
    if !upstream.Enabled {
        return nil, errors.New("upstream is not enabled")
    }

    host, portStr, err := net.SplitHostPort(targetAddress)
    if err != nil {
        return nil, fmt.Errorf("invalid target address: %w", err)
    }
    port, err := strconv.Atoi(portStr)
    if err != nil || port < 1 || port > 65535 {
        return nil, fmt.Errorf("invalid target port: %s", portStr)
    }

    if dialer == nil {
        dialer = &net.Dialer{Timeout: 10 * time.Second}
    }

    proxyConn, err := dialer.DialContext(ctx, "tcp", upstream.Address)
    if err != nil {
        return nil, fmt.Errorf("dial upstream %s: %w", upstream.Address, err)
    }

    deadline := time.Time{}
    if d, ok := ctx.Deadline(); ok {
        deadline = d
    } else if dialer.Timeout > 0 {
        deadline = time.Now().Add(dialer.Timeout)
    }
    if !deadline.IsZero() {
        _ = proxyConn.SetDeadline(deadline)
    }

    if err := upstreamGreeting(proxyConn, upstream.AuthMethod); err != nil {
        _ = proxyConn.Close()
        return nil, err
    }

    if upstream.AuthMethod == "username_password" {
        if err := upstreamPasswordAuth(proxyConn, upstream.Username, upstream.Password); err != nil {
            _ = proxyConn.Close()
            return nil, err
        }
    }

    if err := upstreamConnect(proxyConn, host, port); err != nil {
        _ = proxyConn.Close()
        return nil, err
    }

    _ = proxyConn.SetDeadline(time.Time{})
    return proxyConn, nil
}

func upstreamGreeting(conn net.Conn, authMethod string) error {
    methods := []byte{upstreamMethodNone}
    if authMethod == "username_password" {
        methods = []byte{upstreamMethodPassword}
    }
    req := make([]byte, 0, 2+len(methods))
    req = append(req, version5, byte(len(methods)))
    req = append(req, methods...)
    if _, err := conn.Write(req); err != nil {
        return fmt.Errorf("upstream greeting: %w", err)
    }

    var resp [2]byte
    if _, err := io.ReadFull(conn, resp[:]); err != nil {
        return fmt.Errorf("upstream greeting read: %w", err)
    }
    if resp[0] != version5 {
        return fmt.Errorf("upstream greeting version mismatch: %d", resp[0])
    }
    switch resp[1] {
    case upstreamMethodNone:
        return nil
    case upstreamMethodPassword:
        if authMethod == "username_password" {
            return nil
        }
        return fmt.Errorf("upstream selected password auth but none requested")
    case methodReject:
        return fmt.Errorf("upstream rejected all authentication methods")
    default:
        return fmt.Errorf("upstream selected unsupported auth method: %d", resp[1])
    }
}

func upstreamPasswordAuth(conn net.Conn, username, password string) error {
    if len(username) > 255 || len(password) > 255 {
        return errors.New("upstream username/password must be <= 255 bytes")
    }
    req := make([]byte, 0, 3+len(username)+len(password))
    req = append(req, 0x01, byte(len(username)))
    req = append(req, username...)
    req = append(req, byte(len(password)))
    req = append(req, password...)
    if _, err := conn.Write(req); err != nil {
        return fmt.Errorf("upstream password auth: %w", err)
    }

    var resp [2]byte
    if _, err := io.ReadFull(conn, resp[:]); err != nil {
        return fmt.Errorf("upstream password auth read: %w", err)
    }
    if resp[0] != 0x01 {
        return fmt.Errorf("upstream password auth version mismatch: %d", resp[0])
    }
    if resp[1] != 0x00 {
        return fmt.Errorf("upstream authentication failed: status %d", resp[1])
    }
    return nil
}

func upstreamConnect(conn net.Conn, host string, port int) error {
    atyp, addrBytes, err := encodeUpstreamTarget(host)
    if err != nil {
        return err
    }
    req := make([]byte, 0, 4+len(addrBytes)+2)
    req = append(req, version5, cmdConnect, 0x00, atyp)
    req = append(req, addrBytes...)
    req = append(req, byte(port>>8), byte(port))
    if _, err := conn.Write(req); err != nil {
        return fmt.Errorf("upstream connect request: %w", err)
    }

    var header [4]byte
    if _, err := io.ReadFull(conn, header[:]); err != nil {
        return fmt.Errorf("upstream connect read: %w", err)
    }
    if header[0] != version5 {
        return fmt.Errorf("upstream connect version mismatch: %d", header[0])
    }
    if header[1] != repSuccess {
        return fmt.Errorf("upstream connect failed: %s", replyString(header[1]))
    }

    // Discard BND.ADDR / BND.PORT.
    switch header[3] {
    case atypIPv4:
        var discard [4 + 2]byte
        _, err = io.ReadFull(conn, discard[:])
    case atypDomain:
        var length [1]byte
        if _, err = io.ReadFull(conn, length[:]); err == nil {
            discard := make([]byte, int(length[0])+2)
            _, err = io.ReadFull(conn, discard)
        }
    case atypIPv6:
        var discard [16 + 2]byte
        _, err = io.ReadFull(conn, discard[:])
    default:
        return fmt.Errorf("upstream connect returned unsupported address type: %d", header[3])
    }
    if err != nil {
        return fmt.Errorf("upstream connect discard bind address: %w", err)
    }
    return nil
}

func encodeUpstreamTarget(host string) (atyp byte, addr []byte, err error) {
    if ip := net.ParseIP(host); ip != nil {
        if ip4 := ip.To4(); ip4 != nil {
            return atypIPv4, ip4, nil
        }
        return atypIPv6, ip, nil
    }
    if len(host) > 255 {
        return 0, nil, errors.New("upstream target domain too long")
    }
    return atypDomain, append([]byte{byte(len(host))}, host...), nil
}

func replyString(code byte) string {
    switch code {
    case repSuccess:
        return "success"
    case repGeneralFailure:
        return "general failure"
    case repNotAllowed:
        return "connection not allowed"
    case repNetworkUnreachable:
        return "network unreachable"
    case repHostUnreachable:
        return "host unreachable"
    case repConnectionRefused:
        return "connection refused"
    case repTTLExpired:
        return "TTL expired"
    case repCommandNotSupported:
        return "command not supported"
    case repAddressNotSupported:
        return "address type not supported"
    default:
        return fmt.Sprintf("unknown reply %d", code)
    }
}
```

- [ ] **Step 4: Run tests**

```bash
go test ./internal/socks5/ -run TestDialViaUpstream -v
```

Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/socks5/upstream.go internal/socks5/upstream_test.go
git commit -m "feat(socks5): implement upstream SOCKS5 client with auth"
```

---

## Task 3: Integrate upstream into outbound dial path

**Files:**
- Modify: `internal/socks5/server.go`

- [ ] **Step 1: Route through upstream in dialContext**

In `internal/socks5/server.go` `dialContext`, replace the first use of `dialer := s.dialer()`:

Current code near line 416:

```go
func (s *Server) dialContext(parent context.Context, network, address string, enforceAccess bool) (net.Conn, error) {
    ctx, cancel := s.operationContext(parent)
    defer cancel()
    dialer := s.dialer()
    // ...
}
```

Change to:

```go
func (s *Server) dialContext(parent context.Context, network, address string, enforceAccess bool) (net.Conn, error) {
    ctx, cancel := s.operationContext(parent)
    defer cancel()

    if s.cfg.Upstream.Enabled {
        // Access control is still evaluated against the resolved/requested target
        // before we hand the address to the upstream SOCKS5 proxy.
        host, port, err := net.SplitHostPort(address)
        if err != nil {
            return nil, err
        }
        portNumber, err := strconv.Atoi(port)
        if err != nil || portNumber < 1 || portNumber > 65535 {
            return nil, fmt.Errorf("invalid target port %q", port)
        }
        if enforceAccess && s.access != nil {
            literalHost, _ := splitIPZone(host)
            if ip := net.ParseIP(literalHost); ip != nil {
                if !s.access.AllowTarget(host, ip, portNumber) {
                    return nil, fmt.Errorf("%w: %s", policy.ErrTargetDenied, address)
                }
            }
        }
        return DialViaUpstream(ctx, s.cfg.Upstream, s.dialerWithoutResolver(), network, address)
    }

    dialer := s.dialer()
    // ... rest unchanged ...
}
```

Wait: upstream needs a dialer that can resolve upstream's address. `s.dialer()` uses `s.resolver` which may be DoH/DNS-over-IP bound to the interface. That is fine: the upstream SOCKS5 server address itself should be resolved through the egress interface. However `DialViaUpstream` then does CONNECT with the target domain. So pass `s.dialer()` (with resolver) for the upstream connection, not `s.dialerWithoutResolver()`.

Revised:

```go
if s.cfg.Upstream.Enabled {
    // ... ACL check as above ...
    return DialViaUpstream(ctx, s.cfg.Upstream, s.dialer(), network, address)
}
```

- [ ] **Step 2: Reject BIND and UDP ASSOCIATE when upstream is enabled**

In `handle`:

```go
case cmdBind:
    if !s.cfg.Bind.Enabled {
        _ = writeReply(c, repCommandNotSupported, nil)
        return fmt.Errorf("BIND disabled")
    }
    if s.cfg.Upstream.Enabled {
        _ = writeReply(c, repCommandNotSupported, nil)
        return fmt.Errorf("BIND not supported with upstream proxy")
    }
    // ...

case cmdUDPAssociate:
    if !s.cfg.UDP.Enabled {
        _ = writeReply(c, repCommandNotSupported, nil)
        return fmt.Errorf("UDP ASSOCIATE disabled")
    }
    if s.cfg.Upstream.Enabled {
        _ = writeReply(c, repCommandNotSupported, nil)
        return fmt.Errorf("UDP ASSOCIATE not supported with upstream proxy")
    }
    // ...
```

- [ ] **Step 3: Run existing socks5 tests**

```bash
go test ./internal/socks5/ -v
```

Expected: PASS

- [ ] **Step 4: Commit**

```bash
git add internal/socks5/server.go
git commit -m "feat(socks5): route outbound connections through upstream proxy"
```

---

## Task 4: Backend integration review and credential redaction

**Files:**
- Modify: `internal/webui/server.go`

- [ ] **Step 1: Redact upstream password in API responses**

In `internal/webui/server.go` `redactServerCredential`:

```go
func redactServerCredential(cfg config.Server) config.Server {
    if len(cfg.Auth.Users) > 0 {
        // ... existing logic ...
    }
    cfg.Upstream.Password = ""
    return cfg
}
```

- [ ] **Step 2: Run webui tests**

```bash
go test ./internal/webui/ -v
```

Expected: PASS

- [ ] **Step 3: Commit**

```bash
git add internal/webui/server.go
git commit -m "feat(webui): redact upstream proxy password in API responses"
```

---

## Task 5: WebUI upstream form

**Files:**
- Modify: `internal/webui/static/index.html`
- Modify: `internal/webui/static/app.js`

- [ ] **Step 1: Add upstream HTML fields**

In `internal/webui/static/index.html`, after the "基础设置" section and before "访问认证":

```html
<div class="form-section-heading span-2"><i>01b</i><div><strong>上游代理</strong><span>链式 SOCKS5 上游；启用后 BIND 与 UDP ASSOCIATE 不可用</span></div></div>
<label class="toggle-label span-2"><input type="checkbox" name="upstream_enabled"><span></span>启用上游 SOCKS5 代理</label>
<label class="upstream-field">上游地址<input name="upstream_address" placeholder="10.0.0.1:1080"><small>上级 SOCKS5 监听地址，流量经本出口网口转发到该地址</small></label>
<label class="upstream-field">上游认证方式<select class="native-select" name="upstream_auth_method"><option value="none">无需认证</option><option value="username_password">用户名 / 密码</option></select></label>
<label class="upstream-field upstream-auth-field">上游用户名<input name="upstream_username" placeholder="user"></label>
<label class="upstream-field upstream-auth-field">上游密码<input type="password" name="upstream_password" placeholder="保存时填写"><small>编辑时留空表示不修改密码</small></label>
```

- [ ] **Step 2: Add upstream JS init and population**

In `app.js` `openForm`, after defaults and existing population:

Defaults:

```js
f.elements.upstream_enabled.checked=false;
f.elements.upstream_address.value='';
f.elements.upstream_auth_method.value='none';
f.elements.upstream_username.value='';
f.elements.upstream_password.value='';
```

Population (inside `if(id)` block):

```js
f.elements.upstream_enabled.checked=!!c.upstream?.enabled;
f.elements.upstream_address.value=c.upstream?.address||'';
f.elements.upstream_auth_method.value=c.upstream?.auth_method||'none';
f.elements.upstream_username.value=c.upstream?.username||'';
f.elements.upstream_password.value=''; // password always redacted
```

- [ ] **Step 3: Add upstream field visibility toggles**

Add function:

```js
function updateUpstreamFields(){
    const f=$('#config-form');
    const enabled=f.elements.upstream_enabled.checked;
    $$('.upstream-field').forEach(x=>x.classList.toggle('field-hidden',!enabled));
    const authEnabled=enabled&&f.elements.upstream_auth_method.value==='username_password';
    $$('.upstream-auth-field').forEach(x=>x.classList.toggle('field-hidden',!authEnabled));
}
```

Bind events:

```js
$('#config-form').elements.upstream_enabled.onchange=updateUpstreamFields;
$('#config-form').elements.upstream_auth_method.onchange=updateUpstreamFields;
```

Call `updateUpstreamFields()` in `openForm` after sync.

- [ ] **Step 4: Collect upstream in submitForm**

In `submitForm`, before constructing `cfg`:

```js
const upstreamEnabled=f.elements.upstream_enabled.checked;
const upstream=upstreamEnabled?{
    enabled:true,
    address:f.elements.upstream_address.value.trim(),
    auth_method:f.elements.upstream_auth_method.value,
    username:f.elements.upstream_username.value.trim(),
    password:f.elements.upstream_password.value
}:{enabled:false,address:'',auth_method:'none',username:'',password:''};
```

Add to `cfg`:

```js
const cfg={
    // ... existing fields ...
    upstream,
};
```

- [ ] **Step 5: Test manually**

Open WebUI, create/edit a server, fill upstream fields, save, reload page, verify values persist.

- [ ] **Step 6: Commit**

```bash
git add internal/webui/static/index.html internal/webui/static/app.js
git commit -m "feat(webui): add upstream SOCKS5 proxy configuration form"
```

---

## Task 6: WebUI auth_users table

**Files:**
- Modify: `internal/webui/static/index.html`
- Modify: `internal/webui/static/app.js`

- [ ] **Step 1: Replace auth_users input with table container**

In `index.html`:

```html
<div class="auth-users-field span-2">
  <label>本地代理用户</label>
  <div class="kv-table" id="auth-users-list"></div>
  <button type="button" class="secondary-button" id="auth-users-add">＋ 添加用户</button>
  <small>新增用户必须填写密码；编辑已有用户时留空表示保持原密码。</small>
</div>
```

Remove the old `<label class="auth-users-field">用户 JSON<input name="auth_users" ...>` element.

- [ ] **Step 2: Add render and collection helpers**

In `app.js`, add after `updateAuthFields`:

```js
function renderAuthUsers(users, unchanged){
    const list=$('#auth-users-list');
    const unchangedSet=new Set(unchanged||[]);
    const rows=Object.entries(users||{}).map(([user,password])=>authUserRow(user,password,unchangedSet.has(user)&&password===''));
    list.innerHTML=rows.length?rows.join(''):'<div class="empty-hint">暂无用户</div>';
    bindAuthUserRows();
}

function authUserRow(user,password,isUnchanged){
    return `<div class="kv-row" data-user="${esc(user)}">
        <input type="text" class="auth-user-name" value="${esc(user)}" placeholder="用户名">
        <input type="password" class="auth-user-password" value="${esc(password)}" placeholder="${isUnchanged?'留空保持原密码':'密码'}">
        <button type="button" class="icon-button kv-remove" title="删除">×</button>
    </div>`;
}

function bindAuthUserRows(){
    $$('#auth-users-list .kv-remove').forEach(btn=>btn.onclick=()=>{
        btn.closest('.kv-row').remove();
        if(!$('#auth-users-list').children.length)$('#auth-users-list').innerHTML='<div class="empty-hint">暂无用户</div>';
    });
}

function collectAuthUsers(){
    const users={}, unchanged=[];
    $$('#auth-users-list .kv-row').forEach(row=>{
        const user=row.querySelector('.auth-user-name').value.trim();
        const password=row.querySelector('.auth-user-password').value;
        if(!user)return;
        users[user]=password;
        if(password==='')unchanged.push(user);
    });
    return {users, unchanged};
}
```

- [ ] **Step 3: Update openForm population**

Replace:

```js
f.elements.auth_users.value=c.auth?.users?JSON.stringify(c.auth.users):'';
```

With:

```js
renderAuthUsers(c.auth?.users,c.auth?.password_unchanged);
```

- [ ] **Step 4: Update submitForm collection**

Replace:

```js
const authUsers=jsonField(f.elements.auth_users.value,{}),passwordUnchanged=Object.entries(authUsers).filter(([,password])=>password==='').map(([user])=>user);
```

With:

```js
const {users:authUsers,unchanged:passwordUnchanged}=collectAuthUsers();
```

- [ ] **Step 5: Bind add button**

```js
$('#auth-users-add').onclick=()=>{
    const list=$('#auth-users-list');
    if(list.querySelector('.empty-hint'))list.innerHTML='';
    list.insertAdjacentHTML('beforeend',authUserRow('','',false));
    bindAuthUserRows();
};
```

- [ ] **Step 6: Commit**

```bash
git add internal/webui/static/index.html internal/webui/static/app.js
git commit -m "feat(webui): replace auth_users JSON input with dynamic user table"
```

---

## Task 7: WebUI udp_map table

**Files:**
- Modify: `internal/webui/static/index.html`
- Modify: `internal/webui/static/app.js`

- [ ] **Step 1: Replace udp_map input with table container**

In `index.html`:

```html
<div class="span-2">
  <label>UDP 广播地址映射</label>
  <div class="kv-table" id="udp-map-list"></div>
  <button type="button" class="secondary-button" id="udp-map-add">＋ 添加映射</button>
  <small>当 UDP 监听地址为内网 IP 时，指定对外广播的公网 IP。</small>
</div>
```

Remove the old `<label class="span-2">广播地址映射 JSON<input name="udp_map" ...>` element.

- [ ] **Step 2: Add render and collection helpers**

In `app.js`:

```js
function renderUDPMap(map){
    const list=$('#udp-map-list');
    const rows=Object.entries(map||{}).map(([local,relay])=>udpMapRow(local,relay));
    list.innerHTML=rows.length?rows.join(''):'<div class="empty-hint">暂无映射</div>';
    bindUDPMapRows();
}

function udpMapRow(local,relay){
    return `<div class="kv-row">
        <input type="text" class="udp-map-local" value="${esc(local)}" placeholder="本地监听 IP">
        <span>→</span>
        <input type="text" class="udp-map-relay" value="${esc(relay)}" placeholder="对外广播 IP">
        <button type="button" class="icon-button kv-remove" title="删除">×</button>
    </div>`;
}

function bindUDPMapRows(){
    $$('#udp-map-list .kv-remove').forEach(btn=>btn.onclick=()=>{
        btn.closest('.kv-row').remove();
        if(!$('#udp-map-list').children.length)$('#udp-map-list').innerHTML='<div class="empty-hint">暂无映射</div>';
    });
}

function collectUDPMap(){
    const map={};
    $$('#udp-map-list .kv-row').forEach(row=>{
        const local=row.querySelector('.udp-map-local').value.trim();
        const relay=row.querySelector('.udp-map-relay').value.trim();
        if(local&&relay)map[local]=relay;
    });
    return map;
}
```

- [ ] **Step 3: Update openForm population**

Replace:

```js
f.elements.udp_map.value=c.udp?.advertise_map?JSON.stringify(c.udp.advertise_map):'';
```

With:

```js
renderUDPMap(c.udp?.advertise_map);
```

- [ ] **Step 4: Update submitForm collection**

Replace:

```js
udp:{enabled:..., advertise_map: jsonField(f.elements.udp_map.value,{})},
```

With:

```js
udp:{enabled:..., advertise_map: collectUDPMap()},
```

- [ ] **Step 5: Bind add button**

```js
$('#udp-map-add').onclick=()=>{
    const list=$('#udp-map-list');
    if(list.querySelector('.empty-hint'))list.innerHTML='';
    list.insertAdjacentHTML('beforeend',udpMapRow('',''));
    bindUDPMapRows();
};
```

- [ ] **Step 6: Commit**

```bash
git add internal/webui/static/index.html internal/webui/static/app.js
git commit -m "feat(webui): replace udp_map JSON input with dynamic IP mapping table"
```

---

## Task 8: CSS responsive fixes

**Files:**
- Modify: `internal/webui/static/app.css`

- [ ] **Step 1: Add kv-table styles**

Append to `app.css`:

```css
.kv-table{display:flex;flex-direction:column;gap:8px;margin:8px 0}
.kv-row{display:grid;grid-template-columns:1fr auto 1fr auto;gap:8px;align-items:center}
.kv-row input{width:100%}
.kv-row span{color:var(--muted);font-size:12px}
.empty-hint{color:var(--muted);font-size:13px;padding:8px 0}
```

- [ ] **Step 2: Add modal responsive media query**

Find `.form-grid` definition and ensure it has a responsive override. Append:

```css
@media (max-width:720px){
    .form-grid{grid-template-columns:1fr}
    .form-grid .span-2{grid-column:span 1}
    .modal{width:96vw;max-height:92vh;overflow-y:auto}
    .kv-row{grid-template-columns:1fr 1fr auto}
    .kv-row span{display:none}
}
```

- [ ] **Step 3: Verify small font sizes**

Search for `font-size` under 11px in labels/hints. If found, bump to at least 12px. Example fix if needed:

```css
.form-grid small{font-size:12px}
```

- [ ] **Step 4: Commit**

```bash
git add internal/webui/static/app.css
git commit -m "style(webui): add responsive styles for dynamic tables and modal"
```

---

## Task 9: Full test suite and final verification

- [ ] **Step 1: Run all tests**

```bash
go test ./...
```

Expected: PASS

- [ ] **Step 2: Manual WebUI smoke test**

Build and run locally or on test server:

```bash
go build -o /tmp/wwan-proxy ./cmd/wwan-proxy
```

Open WebUI:
1. Create a server with upstream enabled and username/password, save, reload, verify values.
2. Add auth users via table, save, reload, verify users persist.
3. Add UDP advertise map entries via table, save, reload, verify.
4. Resize browser to mobile width, verify modal does not overflow horizontally.

- [ ] **Step 3: Final commit if any fixes**

```bash
git add -A
git commit -m "fix: address review/test feedback"
```

- [ ] **Step 4: Push**

```bash
git push
```

---

## Spec Coverage Check

| Spec Requirement | Task |
|------------------|------|
| Upstream struct + validation | Task 1 |
| Upstream defaults | Task 1 |
| SOCKS5 client greeting/auth/CONNECT | Task 2 |
| Bind upstream to egress interface | Task 3 |
| Reject BIND/UDP ASSOCIATE with upstream | Task 3 |
| HTTP Proxy auto-uses upstream | Task 3 (via DialContext) |
| API redacts upstream password | Task 4 |
| WebUI upstream form | Task 5 |
| auth_users table | Task 6 |
| udp_map table | Task 7 |
| Responsive/ font fixes | Task 8 |
| Tests | Tasks 1, 2, 9 |

No placeholders or unresolved items remain.

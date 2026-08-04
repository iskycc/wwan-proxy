# wwan-proxy

Linux 多出口 SOCKS5、HTTP/HTTPS Proxy 服务与管理面板。每个代理实例对应一个 `wwan0`、`wwan1`、`wwan2`、`wwan3` 或其他网络接口，外连 TCP、UDP、传统 DNS、DoH 和健康检查均使用 `SO_BINDTODEVICE` 强制从指定接口发出。

## 功能

- SOCKS5 `CONNECT`、`BIND`、`UDP ASSOCIATE`
- HTTP forward proxy 与 HTTPS `CONNECT` 隧道（不解密、不替换目标证书）
- IPv4、IPv6和域名目标
- 无认证或 RFC 1929 用户名密码认证
- 传统 DNS 和 DoH；DNS 请求绑定对应 WWAN
- UDP relay 为每个会话在实例配置范围内随机绑定空闲端口
- 可为每个实例设置心跳 URL、检查周期和超时，所有请求绑定对应 WWAN
- SQLite 持久化配置、结构化日志、错误和心跳状态
- 首次访问初始化管理员，bcrypt 密码哈希与持久化登录会话
- Apple-like 响应式 WebUI，针对 1440p、2K、4K 分级缩放，支持深色模式和细腻的加载/状态动画
- 实时 SOCKS5、HTTP/HTTPS、UDP 会话数与流量，以及 GC live heap、系统内存和 goroutine 指标
- 配置热应用、实例启停、日志搜索、会话管理和故障原因展示
- WebUI 管理系统设置、管理员凭据、登录设备和数据库迁移

## WebUI 预览

首次访问会进入管理员初始化页：

![WWAN Control 管理员初始化](docs/webui-initialization.png)

浅色模式：

![WWAN Control 浅色模式](docs/webui-overview.png)

深色模式会跟随系统自动切换：

![WWAN Control 深色模式](docs/webui-overview-dark.png)

系统、安全与登录会话设置：

![WWAN Control 设置页面](docs/webui-settings.png)

## 编译

需要 Go 1.23、GCC 和 CGO。SQLite 已通过 `go-sqlite3` 编译进程序，不需要独立的 `sqlite3` 命令。

```bash
go test ./...
go build -trimpath -ldflags "-s -w" -o wwan-proxy ./cmd/wwan-proxy
```

## 自动构建与 Release

GitHub Actions 会在每次提交到 `main` 后自动执行以下流程：

1. 运行 `go vet ./...` 和 `go test -race ./...`；
2. 使用 CGO 构建 Linux amd64、Linux arm64，以及不依赖 glibc 的 amd64 musl 静态版本；
3. 打包二进制、README 和 systemd 服务文件；
4. 创建名为 `build-<12 位提交 SHA>` 的预发布 Release，并附带 `SHA256SUMS`。

也可以在仓库的 Actions 页面手动运行同一流程。重复运行同一提交时会更新原 Release 的附件，不会创建重复标签。

### OpenWrt x86_64

OpenWrt 通常使用 musl。普通 `wwan-proxy-linux-amd64` 是 glibc 动态链接版本，在 OpenWrt 上可能出现 `cannot execute: required file not found`，这是系统缺少 glibc 动态加载器造成的。

在 `uname -m` 输出为 `x86_64` 的 OpenWrt 设备上，请下载 Release 中的：

```text
wwan-proxy-linux-amd64-musl.tar.gz
```

该包中的程序通过 musl 完全静态链接，不要求设备安装 glibc 或其他共享库：

```bash
tar -xzf wwan-proxy-linux-amd64-musl.tar.gz
cd wwan-proxy-linux-amd64-musl
chmod +x wwan-proxy
./wwan-proxy -version
./wwan-proxy -db ./wwan-proxy.db
```

代理绑定 WWAN 接口仍需要 root 或对应网络能力。若设备输出为 `aarch64`，不能运行 amd64 包，需要另行提供 arm64 musl 构建。

## 启动

程序不再读取 YAML、TOML 或 JSON 配置文件。首次运行会创建 SQLite 数据库，之后在 WebUI 中添加出口配置。

```bash
sudo ./wwan-proxy \
  -db /var/lib/wwan-proxy/wwan-proxy.db
```

打开：

```text
http://127.0.0.1:9090
```

首次打开 WebUI 时必须创建管理员账号；用户名为 3–64 个字符，密码为 12–72 字节。初始化成功后浏览器会自动登录。

管理员密码仅以 bcrypt 哈希保存。登录使用 32 字节随机令牌，数据库仅保存其 SHA-256 哈希；会话有效期可在“设置”页面修改。Cookie 使用 `HttpOnly` 和 `SameSite=Strict`，在 HTTPS 下还会自动启用 `Secure`。连续登录失败会触发按来源 IP 限速，退出登录会立即删除服务端会话。设置页还支持修改管理员凭据、查看登录设备以及撤销单个或其他全部会话。

默认 WebUI 仅监听本机。若需远程访问，仍建议使用 SSH 端口转发，或通过 HTTPS 反向代理提供服务，避免凭据和 Cookie 经明文 HTTP 传输。

绑定出口接口需要 root 或 `CAP_NET_RAW`：

```bash
sudo setcap cap_net_raw=+ep ./wwan-proxy
./wwan-proxy -db ./wwan-proxy.db
```

`-web` 可作为紧急启动覆盖项，例如 `-web 127.0.0.1:9091`；显式传入时会覆盖 SQLite 中保存的 WebUI 监听地址。正常运行不建议固定传入，以便设置页修改的地址在重启后生效。

## HTTP / HTTPS Proxy

在 WebUI 的出口配置中启用“HTTP / HTTPS Proxy”并设置独立监听地址，例如 `0.0.0.0:8080`。普通 HTTP 请求使用 absolute-form 转发；HTTPS 使用标准 HTTP `CONNECT` 建立端到端 TCP 隧道，程序不执行 TLS 中间人解密，也不生成或替换证书。

HTTP 转发、HTTPS CONNECT 目标连接和域名解析复用该实例的出口拨号器，均绑定配置的 WWAN 接口；选择传统 DNS 或 DoH 时也沿用对应的接口绑定。HTTP Proxy 与 SOCKS5 共用用户名密码、连接超时、空闲超时和并发限制配置。

```bash
# HTTP 目标
curl -x http://proxyuser:password@127.0.0.1:8080 http://ifconfig.me

# HTTPS 目标；curl 自动发送 CONNECT
curl -x http://proxyuser:password@127.0.0.1:8080 https://ifconfig.me
```

HTTP Proxy 监听端口必须与任何实例的 SOCKS5 或已启用 HTTP Proxy 监听端口不同。部署防火墙时需额外开放所配置的 TCP 监听端口。

## SQLite 数据

SQLite 是唯一配置源，主要数据表为：

| 表 | 内容 |
| --- | --- |
| `server_configs` | SOCKS5、HTTP/HTTPS Proxy、认证、DNS、DoH 和 UDP 配置 |
| `event_logs` | INFO、WARN、ERROR、DEBUG 结构化日志及错误上下文 |
| `heartbeat_status` | 每个出口最新心跳、延迟、HTTP 状态、公网 IP、POP 和错误 |
| `admin_users` | 单一 WebUI 管理员账号与 bcrypt 密码哈希 |
| `web_sessions` | 登录会话令牌哈希、到期时间和审计上下文 |
| `system_settings` | WebUI 监听、数据库迁移、日志保留和会话时长 |

数据库启用 WAL、外键和 5 秒 busy timeout。日志保留天数可在 WebUI 修改，保存时立即清理，之后每 6 小时维护一次。配置中的 SOCKS5 用户名和密码会保存在数据库，因此必须保护数据库文件权限。

数据库路径可以在设置页修改。目标必须是尚不存在的绝对路径；程序会在下一次启动时使用 SQLite `VACUUM INTO` 创建一致性副本，切换到新数据库，并更新原始启动数据库中的安全引导指向。迁移期间不会在线切断现有代理连接。

建议备份：

```bash
sudo systemctl stop wwan-proxy
sudo cp /var/lib/wwan-proxy/wwan-proxy.db /safe/path/wwan-proxy.db.backup
sudo systemctl start wwan-proxy
```

## 心跳

每个启用的实例启动后立即检查一次，之后按照该实例配置的周期请求。默认值为：

```text
https://1.1.1.1/cdn-cgi/trace
```

HTTPS socket 在连接前绑定该实例的 `interface`。成功结果保存延迟、公网 IP、Cloudflare POP 和 trace；DNS、TLS、路由、超时、接口不存在、HTTP 非 200 等失败会：

1. 在终端或 journal 打印 `ERROR`；
2. 写入 SQLite `event_logs`，附带实例、接口、URL、延迟和底层错误；
3. 更新 `heartbeat_status`，在 WebUI 显示故障原因。

## DNS 与 DoH

WebUI 可选择系统 DNS、传统 DNS或 DoH。传统 DNS socket 和 DoH HTTPS socket 都绑定实例的 WWAN 接口。

DoH URL 使用域名时必须提供 `bootstrap_ips`，避免先使用系统 DNS解析 DoH 域名。程序直连引导 IP，但 TLS SNI、证书校验和 HTTP Host 仍使用 DoH URL 域名。多个引导 IP 会轮换并在连接失败时依次尝试。

## UDP relay

UDP relay 不提供单一固定端口。每个实例可以设置 relay 端口范围，默认 `10000-65535`。每个 UDP ASSOCIATE 随机选择起点，通过实际 `bind` 原子占用第一个未使用端口，会话结束后释放。部署防火墙需允许所配置的完整范围：

```text
UDP 10000:65535
```

客户端侧和外网侧使用不同 socket；外网侧 socket 绑定对应 WWAN。客户端源 IP 必须与 TCP 控制连接源 IP 一致，源端口在首包时锁定。

## Web API

WebUI 使用以下同源接口：

| 方法 | 地址 | 用途 |
| --- | --- | --- |
| `GET` | `/api/auth/status` | 查询初始化与登录状态 |
| `POST` | `/api/auth/initialize` | 仅首次使用时创建管理员 |
| `POST` | `/api/auth/login` | 管理员登录 |
| `POST` | `/api/auth/logout` | 注销并删除当前会话 |
| `GET/PUT` | `/api/settings` | 查询或修改系统设置 |
| `PUT` | `/api/admin` | 修改管理员用户名或密码 |
| `GET` | `/api/sessions` | 查询有效登录会话 |
| `DELETE` | `/api/sessions/{id}` | 撤销指定登录会话 |
| `POST` | `/api/sessions/revoke-others` | 撤销当前浏览器之外的会话 |
| `GET` | `/api/overview` | 配置、状态、心跳和性能总览 |
| `GET/POST` | `/api/servers` | 查询或创建配置 |
| `PUT/DELETE` | `/api/servers/{id}` | 修改或删除配置 |
| `POST` | `/api/servers/{id}/toggle` | 启用或停用实例 |
| `GET` | `/api/logs` | 查询 SQLite 日志 |
| `GET` | `/api/health` | 管理服务健康状态 |

除健康检查和认证接口外，所有 `/api/*` 接口都要求有效登录会话。修改类请求还会执行同源校验。

## systemd

```bash
sudo useradd --system --no-create-home --shell /usr/sbin/nologin wwan-proxy
sudo install -m 0755 wwan-proxy /usr/local/bin/wwan-proxy
sudo install -m 0644 wwan-proxy.service /etc/systemd/system/wwan-proxy.service
sudo systemctl daemon-reload
sudo systemctl enable --now wwan-proxy
```

服务文件使用 `StateDirectory=wwan-proxy` 自动创建 `/var/lib/wwan-proxy`，并授予进程 `CAP_NET_RAW`。

```bash
journalctl -u wwan-proxy -f
```

## 验证出口

```bash
tcpdump -ni wwan0 host 1.1.1.1
curl --socks5-hostname proxyuser:password@127.0.0.1:1080 https://ifconfig.me
curl -x http://proxyuser:password@127.0.0.1:8080 https://ifconfig.me
```

WebUI 心跳卡片显示的公网 IP 应与该 WWAN 实际出口一致。

后续能力分析和建议优先级见 [功能演进建议](docs/FEATURE-ROADMAP.md)。

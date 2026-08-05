# wwan-proxy

Linux 多出口 SOCKS5、HTTP/HTTPS Proxy 服务与管理面板。每个代理实例可以直接绑定任意已存在的 Linux 物理或虚拟网口，例如 `eth0`、`enp3s0`、`wlan0`、`wwan0`、VLAN、bridge 或 tunnel。外连 TCP、原生 UDP、系统 DNS、传统 DNS、DoH 和健康检查均使用 `SO_BINDTODEVICE` 强制从指定网口发出。

## 功能

- 标准 SOCKS5 `CONNECT`、`BIND` 和 `UDP ASSOCIATE`；`BIND` 默认关闭
- HTTP forward proxy 与 HTTPS `CONNECT` 隧道（不解密、不替换目标证书）
- IPv4、IPv6 和域名目标
- 无认证或 RFC 1929 用户名密码认证
- 系统 DNS、传统 DNS 和 DoH；全部 DNS socket 均绑定对应出口网口
- 每个实例可启用“域名仅解析 IPv4”，开启后只发送 A 查询且不会发送 AAAA 查询
- UDP relay 可从配置范围随机选择端口，或使用多个不连续端口组成的固定池；始终使用原生 UDP，不使用 UDP-over-TCP
- 来源 CIDR admission、按顺序匹配的目标 ACL、全局和每来源 IP 连接/UDP association 限流
- 可为每个实例设置心跳 URL、检查周期和超时，所有请求绑定对应出口网口
- SQLite 持久化配置、结构化日志、错误和心跳状态
- 首次访问初始化管理员；代理凭据使用 bcrypt-sha256、管理员密码使用 bcrypt 哈希存储，并提供持久化登录会话
- Apple-like 响应式 WebUI，针对 1440p、2K、4K 分级缩放，支持深色模式和细腻的加载/状态动画
- 通过会话认证 WebSocket 实时推送 SOCKS5、HTTP/HTTPS、UDP 会话数与流量，以及 GC live heap、系统内存和 goroutine 指标；断线自动退避重连
- 配置热应用、实例启停、日志搜索、会话管理和故障原因展示
- WebUI 管理系统设置、管理员凭据、登录设备和数据库迁移
- WebUI 通过 `/api/interfaces` 发现本机网口，同时允许手工填写网口名

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
2. 使用 CGO 构建 Linux amd64、Linux arm64，以及不依赖 glibc 的 amd64/arm64 musl 静态版本；
3. 打包二进制、README、systemd、OpenWrt procd、Alpine OpenRC 和一键安装脚本；
4. 创建名为 `build-<12 位提交 SHA>` 的正式 Release，并附带 `SHA256SUMS`。

也可以在仓库的 Actions 页面手动运行同一流程。重复运行同一提交时会更新原 Release 的附件，不会创建重复标签。

### Dante 云 NAT 对照代理

需要用 Alpine 原生 Dante 对照测试 SOCKS5 时，可以让 Dante 把云公网 IPv4 当作本机 `internal` 地址，并把云平台送到私网 IPv4 的 TCP/UDP 流量再次 DNAT 到该公网 loopback alias：

```bash
curl -fsSL https://raw.githubusercontent.com/iskycc/wwan-proxy/main/scripts/install-dante-alpine.sh \
  | sh -s -- \
      --public-ip 203.0.113.10 \
      --private-ip 172.16.0.4 \
      --port 18125 \
      --udp-range 16000:17000 \
      --external-iface eth0 \
      --user proxy_user
```

首次创建 `proxy_user` 时会从终端安全调用 `passwd`，也可以通过 `DANTE_PASSWORD` 非交互安装。脚本支持 Alpine 3.21–3.23，安装原生 `dante-server`、OpenRC、iproute2 和 iptables，并完成以下配置：

- 把公网 IPv4 作为 `/32` alias 持久安装到 `lo`；
- 生成 Alpine 使用的 `/etc/sockd.conf`，将 `internal` 设为公网 alias、`external` 设为指定出口网卡；
- 只允许指定系统用户执行 `CONNECT` 和 `UDP ASSOCIATE`，将客户端侧 UDP relay 限制在指定范围；
- 将到达私网 IPv4 的 SOCKS TCP 端口和 UDP relay 范围幂等 DNAT 到公网 alias；
- 通过独立 OpenRC 服务持久化 alias 和 NAT 规则，不覆盖主机已有的 iptables 规则；
- 将 Dante 的文件描述符上限设为 `65536`，进程数限制设为 `unlimited`。

脚本默认不添加可选 SNAT；外部 UDP 实测确认回包源地址异常时，可以使用相同参数重新运行并添加 `--enable-snat`。脚本也不会猜测或改写主机 INPUT 防火墙，云安全组和本机防火墙需要明确放行 TCP `18125` 与 UDP `16000:17000`。原配置会备份到 `/var/backups/dante-cloud/`，完整日志写入 `/var/log/dante-cloud-install.log`。

### Alpine Linux 3.21–3.23 一键安装

Alpine 3.21、3.22 和 3.23 使用 OpenRC。先进入 root shell，再执行全新安装或升级：

```bash
curl -fsSL https://raw.githubusercontent.com/iskycc/wwan-proxy/main/scripts/install-alpine.sh | sh
```

最小 Alpine 默认通常没有 `sudo`；普通用户可先使用系统已有的 `su`、`doas` 或 `sudo` 取得 root shell。安装器启动后才会写诊断日志，因此不要把不存在的 `sudo` 放在上述管道中。

安装器会自动识别 `apk --print-arch`，只选择对应的 musl 静态包：

| Alpine 架构 | Release 资产 |
| --- | --- |
| `x86_64` | `wwan-proxy-linux-amd64-musl.tar.gz` |
| `aarch64` | `wwan-proxy-linux-arm64-musl.tar.gz` |

其他架构会明确退出，不会回退到无法在 Alpine 可靠运行的 glibc 包。安装器还会完成以下工作：

- 严格检查 Alpine `3.21.x`–`3.23.x`、CPU 架构和 Release 包内容；
- 从同一个 Release 下载程序与 `SHA256SUMS`，精确校验目标资产后才解包；
- 安装 OpenRC、CA 证书、`libcap-utils` 和日志轮转依赖，创建非 root 的 `wwan-proxy` 系统用户；
- 将 `cap_net_raw=ep` 只授予服务程序，以支持 `SO_BINDTODEVICE`，并验证 capability 确实生效；
- 将服务的文件描述符上限设为 `65536`、进程/线程 RLIMIT 设为 `unlimited`；容器或宿主机的 cgroup `pids.max` 仍是独立硬上限，安装器不会越权修改；
- 保留 SQLite 数据库和已有 `/etc/conf.d/wwan-proxy`，升级前对默认数据库做安全副本；
- 通过 OpenRC 稳定性检查和 `/api/health` 验证首次启动；程序或服务定义更新失败时自动恢复上一版本，不回退或删除数据库；
- 未在 SQLite 保存监听地址时，以 `0.0.0.0:9090` 作为 Alpine 首次运行默认值，并检查本机防火墙是否可能阻止该 TCP 端口；
- 把每个阶段、命令、退出码、下载状态、SHA-256、系统信息和失败诊断写入 `/var/log/wwan-proxy-install.log`。

固定安装路径：

```text
程序        /usr/local/bin/wwan-proxy
数据库      /var/lib/wwan-proxy/wwan-proxy.db
OpenRC      /etc/init.d/wwan-proxy
服务配置    /etc/conf.d/wwan-proxy
服务日志    /var/log/wwan-proxy/service.log
安装日志    /var/log/wwan-proxy-install.log
升级备份    /var/backups/wwan-proxy/
```

程序、服务账号、数据库、运行目录和日志目录由安装器固定管理，不能在 `conf.d` 中改写；这样可以避免“更新了一个二进制、OpenRC 却启动另一个路径”。`/etc/conf.d/wwan-proxy` 仅用于文件描述符上限、进程/线程 RLIMIT、重启策略和安装后的健康检查地址，升级时保留其内容并强制收紧为 root 所有、不可由普通用户写入。旧配置即使没有 `WWAN_PROXY_NPROC_LIMIT`，新版 OpenRC 也会默认使用 `unlimited`。

常用诊断命令：

```bash
rc-service wwan-proxy status
rc-service wwan-proxy check
rc-service wwan-proxy diagnose
rc-service wwan-proxy log
rc-service wwan-proxy follow

# 需要观察 OpenRC 自身执行细节时
rc-service -d wwan-proxy restart
```

Alpine OpenRC 使用 `-web-default 0.0.0.0:9090`：它只在 SQLite 没有明确值时生效，已保存的 loopback、自定义 IP 或自定义端口不会被覆盖。安装后从可信管理网络访问：

```text
http://<Alpine 主机 IP>:9090
```

安装健康检查仍使用 `127.0.0.1`，不会错误地把通配监听地址 `0.0.0.0` 当成访问目标。升级环境如果使用自定义端口，可以显式指定健康检查地址：

```bash
curl -fsSL https://raw.githubusercontent.com/iskycc/wwan-proxy/main/scripts/install-alpine.sh \
  | sh -s -- --health-url http://127.0.0.1:9191/api/health
```

健康检查 URL 必须是无凭据、无 query 和 fragment 的 `http://` 或 `https://` 地址，避免令牌进入进程参数或诊断日志。显式传入的地址，或 `conf.d` 中不同于默认值的地址，探测失败时会触发程序/OpenRC 文件回滚。

安装器默认审计 firewalld、UFW、awall、nftables 和 iptables，并把完整判断写入安装日志：没有活动 INPUT 防火墙时不会安装防火墙包或添加规则；存在明确规则时记录已放行；确认活动防火墙正在阻断且后端可安全识别时，自动放行实际 WebUI TCP 端口。nftables 自动开放只接受与已安装 Alpine 软件包摘要一致的主配置，并且启动目录中只能有安装器自身管理的片段；其他持久片段、自定义链或无法可靠推导的规则不会被猜测。默认开放模式会发出 WARN 并以非零状态结束，但保留已安装且健康运行的服务，避免日志声称远程可用而实际仍被阻断。云安全组、上级路由器和运营商网络不属于本机规则，仍需单独检查。

首次 WebUI 初始化前，第一个访问者可以创建管理员。默认放通规则面向当前防火墙 zone 或 INPUT 链，安装前应确认主机只连接到可信管理网络；也可以只检查、不更改规则：

```bash
curl -fsSL https://raw.githubusercontent.com/iskycc/wwan-proxy/main/scripts/install-alpine.sh \
  | sh -s -- --check-firewall
```

默认行为不会启用原本停用的防火墙，也不会猜测多个 firewalld zone、awall 拓扑或自定义 nftables 链。可使用 `--open-firewall` 显式选择默认行为，也可用 `--skip-firewall` 完全跳过检查。`0.0.0.0` 会覆盖所有 IPv4 网口，包括可能具有公网地址的 WWAN；应尽快初始化管理员，并优先将 TCP/9090 限制在可信局域网、VPN 或管理源网段。WebUI 当前使用明文 HTTP，不建议直接暴露到互联网。

`--no-start` 不会在服务尚未监听时提前开放端口，因此同时推迟防火墙检查和修改；后续正常运行一次安装器即可启动服务并完成判断。

安装阶段只知道 WebUI 端口。之后在 SQLite 中新增的 SOCKS5、HTTP Proxy 和多个 UDP Relay 端口必须根据实际配置另行放通。

使用本地归档验收时必须同时提供归档和校验文件；安装器不会接受未校验的包：

```bash
sh install-alpine.sh \
  --archive ./wwan-proxy-linux-amd64-musl.tar.gz \
  --checksum ./SHA256SUMS
```

本地归档模式仍会通过 `apk` 检查并按需安装运行依赖；真正断网安装前，需要预先准备可用的本地 apk 仓库或已经安装 `ca-certificates`、`curl`、`libcap-utils`、`logrotate` 和 `openrc`。依赖齐全时安装器会跳过 apk 网络访问。

重复执行同一版本是安全的：数据库和现有服务配置不会被覆盖；程序、OpenRC 定义、权限及 capability 均未变化且服务正在运行时，也不会制造无意义的重启。Alpine 其他版本只能在自行验证后显式传入 `--force-os`。

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

代理绑定指定物理/虚拟网口仍需要 root 或对应网络能力。若设备输出为 `aarch64`，必须改用 `wwan-proxy-linux-arm64-musl.tar.gz`；普通 `wwan-proxy-linux-arm64.tar.gz` 是 glibc 动态版本，不适用于原生 musl 系统。

### OpenWrt procd 守护进程

Release 包内包含 `wwan-proxy.init`。脚本使用以下固定路径：

```text
工作目录  /opt/wwan-proxy
程序      /opt/wwan-proxy/wwan-proxy
数据库    /opt/wwan-proxy/wwan-proxy.db
```

安装或更新时保留已有的 `wwan-proxy.db`：

```bash
mkdir -p /opt/wwan-proxy
cp ./wwan-proxy /opt/wwan-proxy/wwan-proxy
chmod 0755 /opt/wwan-proxy/wwan-proxy

cp ./wwan-proxy.init /etc/init.d/wwan-proxy
chmod 0755 /etc/init.d/wwan-proxy

/etc/init.d/wwan-proxy check
/etc/init.d/wwan-proxy enable
/etc/init.d/wwan-proxy start
/etc/init.d/wwan-proxy status
```

脚本由 procd 管理，文件描述符上限为 65535，异常退出后按 `respawn 3600 5 5` 策略重启，停止时最多等待 15 秒。程序以 root 启动，以便使用 `SO_BINDTODEVICE`；没有传入 `-web`，因此 WebUI 监听地址继续以 SQLite 设置为准。

诊断命令：

```bash
/etc/init.d/wwan-proxy check
/etc/init.d/wwan-proxy log
/etc/init.d/wwan-proxy follow

# 前台调试前先停止后台实例，避免监听端口冲突
/etc/init.d/wwan-proxy stop
/etc/init.d/wwan-proxy run
```

`check` 会实际执行 `wwan-proxy -version`，因此能够识别“文件存在且有执行权限，但 CPU 架构或动态加载器不兼容”的情况。SQLite 数据库不会注册为 procd 文件变更触发器，因为日志和心跳会持续写入该文件；将其作为触发器会造成循环重启。

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

SOCKS5/HTTP Proxy 用户密码写入 SQLite 前会先计算 SHA-256，再使用 bcrypt 哈希；配置 API 不返回哈希，已有明文旧记录在数据库打开时自动迁移并清理旧页与 WAL 中的明文痕迹。这个静态保护不改变代理协议本身：RFC 1929 用户名密码认证和 HTTP Proxy Basic 认证在客户端到代理的链路上都不提供加密。不要把认证监听器直接暴露到不可信网络；应使用可信局域网、VPN、SSH 隧道或其他加密接入，并结合来源 CIDR 和防火墙限制访问。

直接运行二进制、systemd 和 OpenWrt 部署的默认 WebUI 仅监听本机；Alpine OpenRC 在 SQLite 没有保存值时使用 `0.0.0.0:9090`。远程访问仍建议通过可信管理网络、SSH 端口转发或 HTTPS 反向代理，避免凭据和 Cookie 经明文 HTTP 传输。

绑定出口网口需要 root 或 `CAP_NET_RAW`。`interface` 配置填写 Linux 网口名即可，不区分物理网口和虚拟网口；WebUI 会通过 `/api/interfaces` 列出本机候选网口，但也允许直接手工填写：

```bash
sudo setcap cap_net_raw=+ep ./wwan-proxy
./wwan-proxy -db ./wwan-proxy.db
```

启用实例前，服务会预检出口网口是否存在、是否为 `UP` 状态，并用一个最小 socket 实际执行 `SO_BINDTODEVICE`，从而提前发现网口尚未拉起或进程缺少权限等问题。缺失或处于 `DOWN` 状态的网口配置仍可在实例停用时保存；请在网口就绪并授予上述权限后再启用。热更新的预检失败不会中断当前正在工作的旧实例。

`-web` 可作为紧急启动覆盖项，例如 `-web 127.0.0.1:9091`；显式传入时会覆盖 SQLite 中保存的 WebUI 监听地址。正常运行不建议固定传入，以便设置页修改的地址在重启后生效。

## SOCKS5 与访问控制

SOCKS5 服务实现 RFC 1928 的标准 `CONNECT`、`BIND` 和 `UDP ASSOCIATE`。`CONNECT` 默认可用；`BIND` 必须通过实例的 `bind.enabled` 显式开启，旧配置缺少该字段时也保持关闭。BIND 会按请求的 IP、域名和端口约束入站 peer，并发送标准的两次成功响应。`bind.advertise` 默认为 `auto`，优先选择所绑定网口的全局单播地址；多地址或 NAT 环境可显式配置广播 IP。

每个实例可以配置以下访问策略：

- `access.admission_cidrs`：允许连接代理监听器的 IPv4/IPv6 来源网段；留空表示不按 CIDR 限制。
- `access.target_default`：目标 ACL 没有规则命中时执行 `allow` 或 `deny`；旧配置默认为 `allow`。
- `access.target_rules`：按声明顺序匹配，第一条命中规则生效。语法为 `allow|deny target[:port|port-port]`，目标支持域名、`*.example.com`、IP、CIDR 和 `*`；携带端口的 IPv6 目标使用方括号。
- `max_connections`：实例的总并发连接限制。
- `access.max_connections_per_ip`：每个来源 IP 的并发连接限制，`0` 表示不单独限制。
- `udp.max_associations`：实例所有来源合计的 UDP association 硬上限，默认 `64`；热重载的新旧 listener generation 共用同一额度。
- `access.max_udp_associations_per_ip`：每个来源 IP 的 UDP association 限制，`0` 表示不单独限制。

非回环监听、无认证且没有来源 CIDR 限制会形成高风险暴露，WebUI 会显示警告。目标 ACL 和应用层认证不能替代主机防火墙。

## HTTP / HTTPS Proxy

在 WebUI 的出口配置中启用“HTTP / HTTPS Proxy”并设置独立监听地址，例如 `0.0.0.0:8080`。普通 HTTP 请求使用 absolute-form 转发；HTTPS 使用标准 HTTP `CONNECT` 建立端到端 TCP 隧道，程序不执行 TLS 中间人解密，也不生成或替换证书。

HTTP 转发、HTTPS CONNECT 目标连接和域名解析复用该实例的出口拨号器，均绑定配置的出口网口；系统 DNS、传统 DNS 或 DoH 也沿用对应的网口绑定。HTTP Proxy 与 SOCKS5 共用用户名密码、目标 ACL、连接超时、空闲超时和并发限制配置。

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
| `server_configs` | SOCKS5、HTTP/HTTPS Proxy、bcrypt-sha256 代理凭据、ACL、DNS、DoH 和 UDP 配置 |
| `event_logs` | INFO、WARN、ERROR、DEBUG 结构化日志及错误上下文 |
| `heartbeat_status` | 每个出口最新心跳、延迟、HTTP 状态、公网 IP、POP 和错误 |
| `admin_users` | 单一 WebUI 管理员账号与 bcrypt 密码哈希 |
| `web_sessions` | 登录会话令牌哈希、到期时间和审计上下文 |
| `system_settings` | WebUI 监听、数据库迁移、最低日志级别、日志保留和会话时长 |

数据库启用 WAL、外键和 5 秒 busy timeout。系统设置中的最低日志级别支持 `DEBUG`、`INFO`、`WARN`、`ERROR`，默认 `WARN`；例如选择 `WARN` 后，控制台和 SQLite 都只接收 `WARN`、`ERROR`，低级别记录不会先落盘再隐藏。该设置保存后即时作用于已有 logger，无需重启。日志保留天数保存时立即执行过期清理，之后每 6 小时维护一次。SOCKS5/HTTP Proxy 密码以带专用前缀的 bcrypt-sha256 哈希保存在 `config_json`，API 不返回哈希本身；用户名、网络拓扑、ACL 和其他敏感配置仍然可见，因此必须保护数据库文件权限。

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

HTTPS socket 在连接前绑定该实例的 `interface`。成功结果保存延迟、公网 IP、Cloudflare POP 和 trace；DNS、TCP 建连、TLS、响应头、响应体、超时、接口不存在、HTTP 非 200 等失败会：

1. 在终端或 journal 打印 `ERROR`；
2. 写入 SQLite `event_logs`，附带失败阶段与分类、脱敏 URL、目标主机、超时和耗时、完整错误链、DNS 模式与解析地址、连接尝试、TLS 状态，以及出口网口的 flags、MTU、operstate、carrier、地址、丢包/错误计数和该网口的内核路由；
3. 更新 `heartbeat_status`，在 WebUI 显示故障原因。

同一故障只在首次出现、错误或网口/路由状态发生变化时完整记录，持续不变的故障每 5 分钟提醒一次，避免按心跳周期刷屏；恢复事件以 `WARN` 记录，因此最低级别设为 `WARN` 时仍能看到故障与恢复闭环。诊断日志中的心跳和 DoH URL 仅保留 `scheme://host[:port]`，不会记录 userinfo、path、query 或 fragment；DoH 请求头也不会进入诊断日志。

## DNS 与 DoH

WebUI 可选择系统 DNS、传统 DNS 或 DoH。三种模式的 DNS socket 都绑定实例的出口网口。系统 DNS 模式读取主机的 resolver 配置，但使用带 `Dial` hook 的 Go resolver 发起请求，不会退回到可能从默认路由出站的 `net.DefaultResolver` socket。

如果主机的 `/etc/resolv.conf` 指向 `127.0.0.1`、`127.0.0.53` 或 `::1` 等本机 loopback stub，DNS socket 绑定物理/虚拟出口网口后通常无法访问该 loopback 地址。此时应在实例中配置可经该出口到达的传统 DNS 服务器，或配置 DoH；不要假定 systemd-resolved、dnsmasq 等本机 stub 会跨 `SO_BINDTODEVICE` 正常工作。

DoH 可通过 `urls` 配置多个 HTTPS 端点；旧 SQLite 数据中的单个 `url` 会自动兼容。每次缓存未命中的 DNS 查询会同时发给所有 DoH 端点，首个通过 HTTP、DNS 报文、查询 ID、问题区和 RCODE 校验的有效结果胜出，其余请求立即取消。SERVFAIL、REFUSED、截断或问题不匹配的快速响应不会覆盖稍后返回的有效结果。

DoH URL 使用域名时必须提供 `bootstrap_ips`。该兼容字段保存的是 bootstrap DNS 服务器（例如 `114.114.114.114`，默认端口 53），用于先解析 DoH URL 的域名，并不是 DoH 服务器的固定 A 地址。bootstrap DNS 查询、随后建立的 DoH HTTPS 连接都会绑定实例的出口网口；TLS SNI、证书校验和 HTTP Host 仍使用 DoH URL 域名。多个 bootstrap DNS 会轮换；DNS 解析、TCP、TLS、HTTP 或响应校验任一阶段失败时，会在同一次解析的超时预算内切换其他服务器。A/AAAA 并发解析允许返回已经成功的地址族，不会因另一个地址族临时超时而丢弃可用结果。`doh_timeout` 可设置为 `1s`–`2m`，独立于目标 TCP 的 `connect_timeout`。

DoH 结果缓存在实例内存中。正响应使用 Answer 区最小 TTL；NXDOMAIN 和 NODATA 按 SOA TTL 与 SOA.MINIMUM 的较小值执行负缓存。缓存命中时会递减返回报文内各记录的 TTL，并为当前请求重写 DNS ID 和问题区大小写。相同查询的并发缓存未命中会合并为一轮上游竞速，TTL 到期前不会重复请求上游。

每个实例可以通过 WebUI 开启“域名仅解析 IPv4”，对应持久化字段为 `dns.ipv4_only`。开启后 SOCKS5、HTTP/HTTPS Proxy、UDP 和自定义心跳中的域名只发送 A 查询，不发送 AAAA 查询；DoH URL 的域名也只通过 bootstrap DNS 请求 A 记录。IPv4-only DoH 使用直接的 RFC 8484 A 查询，不经过系统解析器的自动重试；失败日志只保留 DoH URL 的 scheme 与 host/port，并记录实际使用的 bootstrap DNS、DoH 端点地址和底层错误。客户端直接提供的 IPv6 字面地址不会经过域名解析，因此不受此开关影响。

## UDP relay

UDP 使用 RFC 1928 标准 `UDP ASSOCIATE` 和独立 UDP socket 转发，不使用 GOST 私有命令，也不会将 UDP 自动封装到 TCP。UDP association 的生命周期绑定到 SOCKS5 TCP 控制连接；控制连接关闭、实例停止或空闲超时后会立即释放 relay socket。

客户端侧 relay 端口支持随机范围和不连续固定端口池两种模式：

- `udp.relay_ports` 非空时使用离散固定端口池，例如 `[12000, 12007, 53000]`。每个 association 通过实际 `bind` 原子占用其中一个空闲端口；选择时从随机位置开始并遍历整个池，所有端口都被占用时返回标准 SOCKS5 失败响应。端口池最多配置 4096 个端口，以限制一次失败请求产生的 bind 尝试；控制连接关闭、超时或实例停止后，端口立即释放并可再次使用。
- `udp.relay_port` 是旧配置兼容字段，`1024`–`65535` 等价于只包含一个端口的池。它不能和 `udp.relay_ports` 同时配置。
- 两个固定端口字段均未配置时，从 `udp.port_min`–`udp.port_max` 范围随机选择并通过实际 `bind` 原子占用，默认范围为 `10000`–`65535`。

随机模式部署防火墙时需允许配置的完整范围，例如：

```text
UDP 10000:65535
```

固定池模式只需逐一放行 `udp.relay_ports` 中配置的 UDP 端口，不要求端口连续。端口池的实际并发容量等于可成功绑定的端口数，通常按期望并发量及可能被其他进程占用的少量余量配置，并让 `udp.max_associations` 不小于期望并发数；没有必要为了并发上限之外的会话配置大量闲置端口。`udp.bind_ip`、`udp.advertise` 和 `udp.advertise_map` 必须保持相同地址族；TCP 控制连接的 peer 地址族也必须与 relay 一致。

`udp.advertise=auto` 在 `bind_ip` 为具体地址时直接广播该地址；只有绑定 `0.0.0.0` 或 `::` 时才使用 SOCKS5 TCP 控制连接的本地地址。NAT 场景可以通过 `advertise` 或 `advertise_map` 明确映射。

客户端侧和外网侧使用不同 socket；外网侧 socket 绑定对应出口网口。客户端源 IP 必须与 TCP 控制连接源 IP 一致，源端口在首包时锁定。默认回包策略锁定客户端联系过且仍在有效期内的目标 IP，同时允许 TFTP 等标准协议从协商后的新端口回复；启用 `udp.strict_endpoint` 后则要求回包 IP 和端口都与客户端发送目标完全一致。当前不重组 SOCKS5 UDP 分片，收到 `FRAG != 0` 的数据报会按 RFC 1928 对不支持分片实现的要求直接丢弃。

固定 relay 端口池没有额外 association token。客户端在 `UDP ASSOCIATE` 中将端口写为 `0` 时，首个匹配 TCP 来源 IP 的 UDP 包会锁定实际客户端端口；因此固定池模式不适合让同一 NAT 来源后的不可信客户端争抢使用。可控客户端应在请求中携带非零 UDP 源端口，并配合来源 CIDR、认证和防火墙。

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
| `GET` | `/api/ws` | WebSocket 实时推送配置、状态、心跳和性能总览 |
| `GET` | `/api/overview` | 配置、状态、心跳和性能总览 |
| `GET` | `/api/interfaces` | 查询本机物理/虚拟网口、MTU、flags 和地址 |
| `GET/POST` | `/api/servers` | 查询或创建配置 |
| `PUT/DELETE` | `/api/servers/{id}` | 修改或删除配置 |
| `POST` | `/api/servers/{id}/toggle` | 启用或停用实例 |
| `GET` | `/api/logs` | 查询 SQLite 日志 |
| `GET` | `/api/health` | 管理服务健康状态 |

除健康检查和认证接口外，所有 `/api/*` 接口都要求有效登录会话。修改类请求和 WebSocket 握手还会执行同源校验。WebUI 的动态总览只使用 `/api/ws`，`/api/overview` 保留供兼容与诊断使用，不参与页面轮询。

配置响应把每个 `auth.users` 密码返回为空字符串，并在 `auth.password_unchanged` 数组中列出对应用户名。更新时原样回传空值和该数组即可保留旧密码；不在数组中的值一律作为新字面密码重新哈希，因此 `"********"` 也可以是真实密码。为兼容旧客户端，仅当请求省略 `password_unchanged`（或传 `null`）时，八个星号仍解释为旧版“保持不变”标记。新客户端应始终显式发送数组，例如：

```json
{
  "auth": {
    "method": "username_password",
    "users": {"alice": "", "bob": "new-password", "stars": "********"},
    "password_unchanged": ["alice"]
  }
}
```

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

WebUI 心跳卡片显示的公网 IP 应与该网口的实际出口一致。

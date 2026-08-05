# wwan-proxy 连接卡顿问题报告

## 1. 问题摘要

- **现象**：从特定客户端 IP `110.53.182.21` 访问 SOCKS5 代理端口 `23.141.204.202:59999` 时，连接建立缓慢或长时间无响应（curl 135 秒超时）。其他源 IP 访问同一端口正常。
- **服务**：`wwan-proxy` v`624c2ee9de13`，Alpine Linux 3.23.5，OpenRC 托管。
- **初步结论**：问题极可能源于 **容器内存硬限制仅 192 MB**，在高并发/大流量场景下导致 TCP 发送缓冲区不足、连接关闭异常。不是 wwan-proxy 代码逻辑 bug，也不是本机防火墙或单纯被厂商拉黑。

---

## 2. 环境信息

| 项目 | 值 |
|------|-----|
| 主机名 | `u5463-ev8keqn0` |
| 内核 | `Linux 5.15.0-186-generic #196-Ubuntu SMP` |
| 发行版 | Alpine Linux v3.23.5 |
| wwan-proxy 版本 | `624c2ee9de13` |
| 监听地址 | `*:59999`（SOCKS5）、`0.0.0.0:9090`（WebUI） |
| 容器内存硬限制 | `192000000`（192 MB） |
| wwan-proxy RSS | 约 14–17 MB |
| 进程打开 FD | 108 / 65536 |

---

## 3. 现象描述

1. **端口监听正常**：`ss` 确认 `59999` 处于 `LISTEN` 状态。
2. **特定 IP 异常**：所有异常连接均来自 `110.53.182.21`。
3. **大量 `FIN-WAIT-1`**：该源 IP 在 `59999` 端口上留下 60+ 个 `FIN-WAIT-1` 状态连接。
4. **`Send-Q` 堆积**：单个连接 `Send-Q` 堆积高达 130 KB。
   ```
   ESTAB 0 130427 [::ffff:10.10.2.65]:59999 [::ffff:110.53.182.21]:14499
   ```
5. **日志高频报错**：大量 `read tcp ...: i/o timeout`、`read: connection timed out`、`write: broken pipe`。
6. **其他 IP 正常**：用户确认从其他 IP 访问 `59999` 无异常。
7. **反向 ICMP 正常**：从服务器 `ping`/`traceroute` 到 `110.53.182.21` 可达，延迟约 193 ms，无丢包。

---

## 4. 关键证据

### 4.1 连接状态分布（针对 110.53.182.21）

```
67 FIN-WAIT-1
35 ESTAB
 2 LAST-ACK
 1 LISTEN
```

### 4.2 TCP 统计

```
Tcp: 1 200 120000 -1 1161 2027 0 234 59 148627 164401 4612 7 293 0
```

### 4.3 容器内存限制

```
/sys/fs/cgroup/memory.max = 192000000
```

### 4.4 资源使用（宿主机视角，非容器实际可用）

```
Mem: 7936 total, 6626 used, 1270 free, 167 available
```

注意：`free` 在容器内显示的是宿主机内存，容器的实际可用上限由 cgroup `memory.max` 决定。

### 4.5 防火墙/内核

- 本机无 active INPUT 防火墙（`iptables`、`nftables`、`ufw` 均未启用）。
- `dmesg` 中无 SYN flood、conntrack full、IP drop 记录。
- `nf_conntrack_count = 0`。

---

## 5. 根因分析

### 5.1 最可能原因：容器内存瓶颈

容器被硬限制为 **192 MB**。虽然 `wwan-proxy` 进程 RSS 仅约 17 MB，但每个 SOCKS5/出站 TCP 连接都需要内核 socket buffer：

- 发送缓冲区（`tcp_wmem`）
- 接收缓冲区（`tcp_rmem`）
- SOCKS5 客户端连接 + 代理目标连接成对出现

当 `110.53.182.21` 发起大量并发连接或传输大流量时，这些缓冲区会迅速占用内存。接近 192 MB 上限后，内核进入内存压力状态，导致：

1. TCP 数据无法及时发出 → `Send-Q` 堆积。
2. 客户端等不到 ACK → 重传/重连，端口数暴涨。
3. 服务器主动关闭连接时，FIN 发不出去或等不到 ACK → 大量 `FIN-WAIT-1`。
4. 新连接建立变慢，表现为 curl 135 秒超时。

### 5.2 为什么只有 110.53.182.21 受影响？

该 IP 是当前主要/高流量客户端。其他 IP 连接数少、流量小，不会触发内存压力。

### 5.3 为什么不是厂商拉黑？

- 反向 `ping`/`traceroute` 正常。
- 其他 IP 可正常连接同一端口。
- 本机防火墙无规则，内核无 drop 日志。
- 如果是黑名单，应表现为完全不可达或 SYN 被静默丢弃，而不是大量半关闭连接和 Send-Q 堆积。

### 5.4 为什么不是 wwan-proxy 代码 bug？

- 服务对其他客户端正常。
- 进程 RSS 低，FD 使用率低，未触发文件描述符上限。
- 错误类型集中在网络 I/O timeout / broken pipe，符合资源受限特征。

---

## 6. 已排除的原因

| 原因 | 排除依据 |
|------|----------|
| 端口未监听 | `ss -antl` 显示 `*:59999` 正常监听 |
| wwan-proxy 崩溃 | 进程 PID 稳定，`rc-service` 状态正常 |
| 文件描述符耗尽 | 已用 108，限制 65536 |
| 本机防火墙阻断 | 无 active iptables/nftables/ufw |
| SYN flood 触发 | `dmesg` 无相关日志，`nf_conntrack_count=0` |
| 宿主机内存耗尽 | 容器有独立 192 MB 上限，与宿主机 `free` 无关 |
| 客户端 IP 被完全拉黑 | 反向 ping/traceroute 正常 |

---

## 7. 建议解决方案

### 7.1 首选：扩容容器内存

将该 LXC/Incus 容器内存限制从 192 MB 提升到 **512 MB 或 1 GB**。

示例命令（宿主机执行）：

```bash
# Incus / LXD
incus config set u5463-ev8keqn0 limits.memory=1GB
incus restart u5463-ev8keqn0
```

如果是在 hosting 控制台购买的容器，请在控制台调整内存规格。

### 7.2 验证扩容效果

重启后运行诊断脚本：

```bash
bash <(curl -fsSL https://raw.githubusercontent.com/iskycc/wwan-proxy/main/scripts/debug-alpine.sh)
```

确认：
- `/sys/fs/cgroup/memory.max` 已变大。
- `FIN-WAIT-1` 数量不再持续增加。
- `Send-Q` 无异常堆积。
- curl 通过 SOCKS5 访问正常。

### 7.3 备选：进一步抓包确认

若扩容后问题依旧，安装 tcpdump 并在卡住时抓包：

```bash
apk add tcpdump
timeout 30 tcpdump -i any -n -s 0 "host 110.53.182.21 and port 59999" -c 500 -w /tmp/capture.pcap
```

分析是否出现：服务器发 `SYN-ACK` 后收不到 `ACK`、大量 TCP retransmission、或窗口被压到很小。

---

## 8. 相关日志片段

```
time=2026-08-05T08:57:12.969Z level=WARN msg="SOCKS5 connection ended with error" component=manager server=eth0 interface=eth0 remote=110.53.182.21:9567 error="read tcp 10.10.2.65:59999->110.53.182.21:9567: i/o timeout"
time=2026-08-05T08:57:28.852Z level=WARN msg="SOCKS5 connection ended with error" component=manager server=eth0 interface=eth0 remote=110.53.182.21:7341 error="read tcp4 10.10.2.65:44144->34.120.208.123:443: i/o timeout"
time=2026-08-05T08:57:47.316Z level=WARN msg="SOCKS5 connection ended with error" component=manager server=eth0 interface=eth0 remote=110.53.182.21:10276 error="read tcp 10.10.2.65:59999->110.53.182.21:10276: i/o timeout"
```

---

## 9. 待办

- [ ] 将容器内存从 192 MB 扩容到 512 MB / 1 GB
- [ ] 扩容后重新运行 `debug-alpine.sh` 验证
- [ ] 如仍有问题，安装 tcpdump 抓包并补充到本报告

#!/bin/sh
# wwan-proxy 故障定位脚本（Alpine/BusyBox 兼容）
# 输出到 /tmp/wwan-proxy-debug-YYYYMMDD-HHMMSS.log
#
# 用法：
#   bash <(curl -fsSL https://raw.githubusercontent.com/iskycc/wwan-proxy/main/scripts/debug-alpine.sh)
#
# 针对单一客户端 IP 卡顿时，可指定该 IP：
#   PROBLEM_IP=110.53.182.21 bash <(curl -fsSL ...)

set -u

PROBLEM_IP="${PROBLEM_IP:-110.53.182.21}"
OUT="/tmp/wwan-proxy-debug-$(date -u '+%Y%m%d-%H%M%S').log"
PCAP="/tmp/wwan-proxy-debug-$(date -u '+%Y%m%d-%H%M%S')-${PROBLEM_IP}.pcap"
SOCKS_PORT="59999"
WEB_PORT="9090"
DB="/var/lib/wwan-proxy/wwan-proxy.db"
LOG="/var/log/wwan-proxy/service.log"
INSTALL_LOG="/var/log/wwan-proxy-install.log"

echo "正在收集诊断信息，输出到 ${OUT} ..."
exec >"${OUT}" 2>&1

section() {
  echo ""
  echo "========================================"
  echo "$1"
  echo "========================================"
}

run() {
  echo ""
  echo "\$ $*"
  "$@" 2>&1 || echo "[command exited with $?]"
}

section "基本环境"
echo "PROBLEM_IP=${PROBLEM_IP}"
run date -u
run uname -a
run cat /etc/os-release
run uptime

section "wwan-proxy 进程"
run ps | grep -i '[w]wan-proxy'
run pidof wwan-proxy
PID=$(pidof wwan-proxy 2>/dev/null || true)
if [ -n "${PID}" ]; then
  run ls -l /proc/${PID}/exe
  run /proc/${PID}/exe -version
  run cat /proc/${PID}/status | grep -E '^(Pid|PPid|Threads|FDSize|VmRSS|VmSize|State)'
  run ls /proc/${PID}/fd | wc -l
  run cat /proc/${PID}/limits | grep -i 'open files'
fi

section "网络连接状态 (${SOCKS_PORT})"
run ss -ant | grep ":${SOCKS_PORT}" | head -100
run ss -ant | grep ":${SOCKS_PORT}" | awk '{print $1}' | sort | uniq -c | sort -rn
run ss -antl | grep ":${SOCKS_PORT}"
run ss -s

section "问题源 IP 概览 (${PROBLEM_IP})"
run ss -ant | grep "${PROBLEM_IP}"
run ss -ant | grep "${PROBLEM_IP}" | awk '{print $1}' | sort | uniq -c | sort -rn
echo ""
echo "该源 IP 的发送队列堆积（Send-Q）Top 20:"
ss -ant | grep "${PROBLEM_IP}" | awk '{print $3, $4}' | sort -k2 -n | tail -20 || true
echo ""
echo "该源 IP 的 Recv-Q 堆积 Top 20:"
ss -ant | grep "${PROBLEM_IP}" | awk '{print $2, $4}' | sort -k1 -n | tail -20 || true

section "问题源 IP 抓包 (${PROBLEM_IP})"
if command -v tcpdump >/dev/null 2>&1; then
  echo "将抓取 30 秒或 500 个包，保存到 ${PCAP}"
  echo "（请在卡住时立即运行此脚本，或在本脚本运行期间从该 IP 发起测试）"
  run timeout 30 tcpdump -i any -n -s 0 "host ${PROBLEM_IP} and port ${SOCKS_PORT}" -c 500 -w "${PCAP}"
  if [ -f "${PCAP}" ]; then
    echo ""
    echo "PCAP 已保存: ${PCAP}"
    echo "简要解读（仅前 20 条）:"
    run timeout 10 tcpdump -nn -r "${PCAP}" -c 20
  fi
else
  echo "tcpdump 未安装，跳过抓包。Alpine 安装命令: apk add tcpdump"
fi

section "到问题源 IP 的反向连通性"
if command -v ping >/dev/null 2>&1; then
  run ping -c 10 -W 2 "${PROBLEM_IP}"
fi
if command -v mtr >/dev/null 2>&1; then
  run mtr -r -c 10 "${PROBLEM_IP}"
elif command -v traceroute >/dev/null 2>&1; then
  run traceroute -n -m 20 -w 2 "${PROBLEM_IP}"
fi

section "TCP 统计与丢包"
run cat /proc/net/snmp | grep -E '^Tcp:' | tail -1
run cat /proc/net/netstat 2>/dev/null | grep -E '^TcpExt:' | tail -1
run cat /proc/net/snmp6 2>/dev/null | grep -E '^Tcp[^ ]*' | head -20

section "按源 IP 聚合连接数"
run ss -ant | awk '{print $5}' | awk -F: '{print $1}' | sort | uniq -c | sort -rn | head -20

section "WebUI 端口 (${WEB_PORT})"
run ss -antl | grep ":${WEB_PORT}"

section "连接追踪 (conntrack)"
if command -v conntrack >/dev/null 2>&1; then
  run conntrack -L 2>/dev/null | wc -l
  run conntrack -L 2>/dev/null | grep ":${SOCKS_PORT}" | head -30
  run conntrack -L 2>/dev/null | grep "${PROBLEM_IP}" | head -30
elif [ -f /proc/sys/net/netfilter/nf_conntrack_count ]; then
  run cat /proc/sys/net/netfilter/nf_conntrack_count
  run cat /proc/sys/net/netfilter/nf_conntrack_max
else
  echo "conntrack 不可用"
fi

section "SYN flood / TCP 设置"
for f in /proc/sys/net/ipv4/tcp_syncookies /proc/sys/net/ipv4/tcp_max_syn_backlog /proc/sys/net/ipv4/tcp_synack_retries /proc/sys/net/ipv4/tcp_keepalive_time /proc/sys/net/ipv4/tcp_keepalive_probes /proc/sys/net/ipv4/tcp_keepalive_intvl /proc/sys/net/ipv4/tcp_retries1 /proc/sys/net/ipv4/tcp_retries2 /proc/sys/net/core/netdev_max_backlog; do
  if [ -f "$f" ]; then
    echo "$f = $(cat "$f")"
  fi
done

section "内核日志 (SYN flood / drop / 问题 IP 相关)"
if command -v dmesg >/dev/null 2>&1; then
  run dmesg 2>/dev/null | grep -iE 'syn|drop|conntrack|nf_' | tail -50
  echo ""
  echo "内核日志中是否提到问题 IP ${PROBLEM_IP}:"
  run dmesg 2>/dev/null | grep "${PROBLEM_IP}" | tail -30
else
  echo "dmesg 不可用"
fi

section "防火墙规则"
if command -v nft >/dev/null 2>&1; then
  run nft list ruleset 2>/dev/null | head -200
  echo ""
  echo "nftables 中是否提到问题 IP ${PROBLEM_IP}:"
  run nft list ruleset 2>/dev/null | grep "${PROBLEM_IP}" | head -30
fi
if command -v iptables >/dev/null 2>&1; then
  run iptables -L -n -v 2>/dev/null | head -100
  echo ""
  echo "iptables 中是否提到问题 IP ${PROBLEM_IP}:"
  run iptables -L -n -v 2>/dev/null | grep "${PROBLEM_IP}" | head -30
fi
if command -v ufw >/dev/null 2>&1; then
  run ufw status verbose 2>/dev/null
fi

section "wwan-proxy 配置"
if [ -f "${DB}" ]; then
  if command -v sqlite3 >/dev/null 2>&1; then
    run sqlite3 "${DB}" "SELECT id, name, enabled, listen, interface, json_extract(config, '\$.max_connections') AS max_conn, json_extract(config, '\$.idle_timeout') AS idle, json_extract(config, '\$.connect_timeout') AS conn_to, json_extract(config, '\$.udp.enabled') AS udp_en, json_extract(config, '\$.udp.advertise') AS udp_adv, json_extract(config, '\$.udp.bind_ip') AS udp_bind, json_extract(config, '\$.bind.enabled') AS bind_en, json_extract(config, '\$.bind.advertise') AS bind_adv FROM servers;"
    run sqlite3 "${DB}" "SELECT id, name, config FROM servers WHERE listen LIKE '%${SOCKS_PORT}%';"
  elif command -v python3 >/dev/null 2>&1; then
    run python3 -c "import sqlite3,json;c=sqlite3.connect('${DB}');print('id|name|enabled|listen|interface|max_conn|idle|conn_to|udp_en|udp_adv|udp_bind|bind_en|bind_adv');[print('|'.join(str(x) for x in r[:4])+('|'+r[4] if r[4] else '|')+'|'+str(json.loads(r[5]).get('max_connections',''))+'|'+json.loads(r[5]).get('idle_timeout','')+'|'+json.loads(r[5]).get('connect_timeout','')+'|'+str(json.loads(r[5]).get('udp',{}).get('enabled',''))+'|'+json.loads(r[5]).get('udp',{}).get('advertise','')+'|'+json.loads(r[5]).get('udp',{}).get('bind_ip','')+'|'+str(json.loads(r[5]).get('bind',{}).get('enabled',''))+'|'+json.loads(r[5]).get('bind',{}).get('advertise','')) for r in c.execute('SELECT id,name,enabled,listen,interface,config FROM servers')]"
  else
    echo "sqlite3/python3 均不可用，无法读取数据库配置"
  fi
else
  echo "数据库不存在: ${DB}"
fi

section "wwan-proxy 日志 (最近 200 行)"
if [ -f "${LOG}" ]; then
  run tail -n 200 "${LOG}"
else
  echo "日志不存在: ${LOG}"
fi

section "安装日志 (最近 100 行)"
if [ -f "${INSTALL_LOG}" ]; then
  run tail -n 100 "${INSTALL_LOG}"
else
  echo "安装日志不存在: ${INSTALL_LOG}"
fi

section "接口和路由"
run ip addr show 2>/dev/null || ifconfig -a 2>/dev/null
run ip route show 2>/dev/null || route -n 2>/dev/null

section "内存占用大户"
run ps -o pid,rss,comm,args 2>/dev/null | sort -k2 -n | tail -20

section "容器内存限制"
for f in /sys/fs/cgroup/memory.max /sys/fs/cgroup/memory.limit_in_bytes /sys/fs/cgroup/memory/memory.limit_in_bytes; do
  if [ -f "$f" ]; then
    echo "$f = $(cat "$f")"
  fi
done
run cat /sys/fs/cgroup/memory.stat 2>/dev/null | head -20 || true

section "资源使用"
run free -m 2>/dev/null || cat /proc/meminfo | head -10
run df -h 2>/dev/null | head -10

section "完成"
echo ""
echo "诊断收集完成: ${OUT}"
echo "如果抓了包，PCAP 文件在: ${PCAP}"
echo "请把日志文件内容（必要时连同 PCAP）贴给开发者或厂商。"

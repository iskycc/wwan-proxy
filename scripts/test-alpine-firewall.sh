#!/usr/bin/env bash

set -euo pipefail

if [[ $# -ne 2 ]]; then
  echo "usage: $0 ARCHIVE SHA256SUMS" >&2
  exit 2
fi

repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
archive=$(realpath "$1")
checksums=$(realpath "$2")
case "${archive}" in
  "${repo_root}"/*) ;;
  *) echo "archive must be inside ${repo_root}: ${archive}" >&2; exit 2 ;;
esac
case "${checksums}" in
  "${repo_root}"/*) ;;
  *) echo "checksum file must be inside ${repo_root}: ${checksums}" >&2; exit 2 ;;
esac

archive_in_container="/workspace/${archive#"${repo_root}"/}"
checksums_in_container="/workspace/${checksums#"${repo_root}"/}"
container_name="wwan-proxy-alpine-firewall-$$"
iptables_container_name="${container_name}-iptables"
firewalld_container_name="${container_name}-firewalld"
ufw_container_name="${container_name}-ufw"

cleanup() {
  docker rm --force "${container_name}" >/dev/null 2>&1 || true
  docker rm --force "${iptables_container_name}" >/dev/null 2>&1 || true
  docker rm --force "${firewalld_container_name}" >/dev/null 2>&1 || true
  docker rm --force "${ufw_container_name}" >/dev/null 2>&1 || true
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

echo "[firewall-test] starting privileged Alpine 3.23/OpenRC container ${container_name}"
docker run --detach --privileged \
  --name "${container_name}" \
  --volume "${repo_root}:/workspace:ro" \
  alpine:3.23 \
  sh -ec 'apk add --no-cache openrc nftables awall >/tmp/bootstrap.log 2>&1; exec /sbin/init' \
  >/dev/null

for attempt in $(seq 1 30); do
  if docker exec "${container_name}" rc-status --all >/dev/null 2>&1; then
    break
  fi
  if [[ ${attempt} -eq 30 ]]; then
    docker logs "${container_name}" >&2 || true
    exit 1
  fi
  sleep 1
done

docker exec "${container_name}" rc-update add nftables default >/dev/null
docker exec "${container_name}" rc-service nftables start >/dev/null
if [[ -n $(docker exec "${container_name}" awall list) ]]; then
  echo "inactive awall unexpectedly has an enabled optional policy" >&2
  exit 1
fi

container_ipv4=$(docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' "${container_name}")
test -n "${container_ipv4}"

echo "[firewall-test] check mode must report DROP without changing nftables"
docker exec "${container_name}" sh /workspace/scripts/install-alpine.sh \
  --archive "${archive_in_container}" \
  --checksum "${checksums_in_container}" \
  --check-firewall \
  >/tmp/wwan-proxy-alpine-firewall-check.log
if docker exec "${container_name}" test -e /etc/nftables.d/50_wwan-proxy.nft; then
  echo "check mode unexpectedly created an nftables fragment" >&2
  exit 1
fi
if curl --noproxy '*' --fail --silent --show-error --connect-timeout 2 --max-time 3 \
  "http://${container_ipv4}:9090/api/health" >/dev/null 2>&1; then
  echo "default-DROP firewall unexpectedly allowed WebUI before open mode" >&2
  exit 1
fi

echo "[firewall-test] default open mode must add one persistent/live rule"
docker exec "${container_name}" sh /workspace/scripts/install-alpine.sh \
  --archive "${archive_in_container}" \
  --checksum "${checksums_in_container}" \
  >/tmp/wwan-proxy-alpine-firewall-open.log
docker exec "${container_name}" grep -Fqx '# Managed by the wwan-proxy Alpine installer.' \
  /etc/nftables.d/50_wwan-proxy.nft
test "$(docker exec "${container_name}" nft -a list chain inet filter input | grep -c 'accept wwan-proxy WebUI')" -eq 1
curl --noproxy '*' --fail --silent --show-error --connect-timeout 2 --max-time 3 \
  "http://${container_ipv4}:9090/api/health" >/dev/null
docker exec "${container_name}" rc-service nftables restart >/dev/null
curl --noproxy '*' --fail --silent --show-error --connect-timeout 2 --max-time 3 \
  "http://${container_ipv4}:9090/api/health" >/dev/null

echo "[firewall-test] repeat install must be idempotent and preserve runtime-only rules"
docker exec "${container_name}" nft \
  'add rule inet filter input udp dport 19001 accept comment "wwan-proxy CI runtime-only"'
docker exec "${container_name}" sh /workspace/scripts/install-alpine.sh \
  --archive "${archive_in_container}" \
  --checksum "${checksums_in_container}" \
  >/tmp/wwan-proxy-alpine-firewall-repeat.log
test "$(docker exec "${container_name}" nft -a list chain inet filter input | grep -c 'accept wwan-proxy WebUI')" -eq 1
docker exec "${container_name}" nft -a list chain inet filter input | grep -Fq 'wwan-proxy CI runtime-only'

echo "[firewall-test] an enabled but stopped nftables service must fail closed"
docker exec "${container_name}" rc-service nftables stop >/dev/null
if docker exec "${container_name}" sh /workspace/scripts/install-alpine.sh \
  --archive "${archive_in_container}" \
  --checksum "${checksums_in_container}" \
  >/tmp/wwan-proxy-alpine-firewall-stopped.log 2>&1; then
  echo "installer unexpectedly trusted an enabled but stopped nftables service" >&2
  exit 1
fi
grep -Fq 'an OpenRC firewall service is enabled but not active' \
  /tmp/wwan-proxy-alpine-firewall-stopped.log
docker exec "${container_name}" rc-service nftables start >/dev/null
curl --noproxy '*' --fail --silent --show-error --connect-timeout 2 --max-time 3 \
  "http://${container_ipv4}:9090/api/health" >/dev/null

echo "[firewall-test] live ACCEPT must not skip persistence of the next DROP-policy reload"
docker exec "${container_name}" sh -ec '
  rm -f /etc/nftables.d/50_wwan-proxy.nft
  nft "add chain inet filter input { policy accept; }"
'
docker exec "${container_name}" sh /workspace/scripts/install-alpine.sh \
  --archive "${archive_in_container}" \
  --checksum "${checksums_in_container}" \
  >/tmp/wwan-proxy-alpine-firewall-live-accept.log
docker exec "${container_name}" grep -Fqx '# Managed by the wwan-proxy Alpine installer.' \
  /etc/nftables.d/50_wwan-proxy.nft
docker exec "${container_name}" nft 'add chain inet filter input { policy drop; }'
curl --noproxy '*' --fail --silent --show-error --connect-timeout 2 --max-time 3 \
  "http://${container_ipv4}:9090/api/health" >/dev/null

echo "[firewall-test] a started nftables service with an empty live ruleset must fail before restart"
docker exec "${container_name}" sh -ec '
  rm -f /etc/nftables.d/50_wwan-proxy.nft
  nft flush ruleset
'
if docker exec "${container_name}" sh /workspace/scripts/install-alpine.sh \
  --archive "${archive_in_container}" \
  --checksum "${checksums_in_container}" \
  >/tmp/wwan-proxy-alpine-firewall-empty-live.log 2>&1; then
  echo "installer unexpectedly trusted an empty live ruleset owned by started nftables" >&2
  exit 1
fi
grep -Fq 'nftables OpenRC reports started but the live ruleset is empty' \
  /tmp/wwan-proxy-alpine-firewall-empty-live.log
curl --noproxy '*' --fail --silent --show-error --connect-timeout 2 --max-time 3 \
  "http://${container_ipv4}:9090/api/health" >/dev/null
docker exec "${container_name}" rc-service nftables restart >/dev/null
if curl --noproxy '*' --fail --silent --show-error --connect-timeout 2 --max-time 3 \
  "http://${container_ipv4}:9090/api/health" >/dev/null 2>&1; then
  echo "stock nftables DROP policy unexpectedly allowed WebUI after restart" >&2
  exit 1
fi
docker exec "${container_name}" sh /workspace/scripts/install-alpine.sh \
  --archive "${archive_in_container}" \
  --checksum "${checksums_in_container}" \
  >/tmp/wwan-proxy-alpine-firewall-empty-recovery.log
curl --noproxy '*' --fail --silent --show-error --connect-timeout 2 --max-time 3 \
  "http://${container_ipv4}:9090/api/health" >/dev/null

echo "[firewall-test] pending terminal flow in nftables.nft must fail before the next reload"
docker exec "${container_name}" sh -ec '
  cp -p /etc/nftables.nft /tmp/nftables.nft.before-pending-test
  sed -i '\''/^# Rules$/i add rule inet filter input ip saddr 203.0.113.0/24 drop comment "wwan-proxy CI pending terminal"'\'' /etc/nftables.nft
  nft -c -f /etc/nftables.nft
'
if docker exec "${container_name}" sh /workspace/scripts/install-alpine.sh \
  --archive "${archive_in_container}" \
  --checksum "${checksums_in_container}" \
  >/tmp/wwan-proxy-alpine-firewall-pending.log 2>&1; then
  echo "installer unexpectedly accepted pending terminal flow in nftables.nft" >&2
  exit 1
fi
grep -Fq '/etc/nftables.nft contains persistent input control flow' \
  /tmp/wwan-proxy-alpine-firewall-pending.log
docker exec "${container_name}" mv -f /tmp/nftables.nft.before-pending-test /etc/nftables.nft

echo "[firewall-test] modified nftables.nft must leave automatic mode even when the edit looks harmless"
docker exec "${container_name}" sh -ec '
  cp -p /etc/nftables.nft /tmp/nftables.nft.before-audit-test
  printf '\''%s\n'\'' '\''# wwan-proxy CI package-audit regression'\'' >>/etc/nftables.nft
  nft -c -f /etc/nftables.nft
'
if docker exec "${container_name}" sh /workspace/scripts/install-alpine.sh \
  --archive "${archive_in_container}" \
  --checksum "${checksums_in_container}" \
  >/tmp/wwan-proxy-alpine-firewall-apk-audit.log 2>&1; then
  echo "installer unexpectedly trusted a modified nftables.nft" >&2
  exit 1
fi
grep -Fq '/etc/nftables.nft differs from the Alpine package-managed baseline' \
  /tmp/wwan-proxy-alpine-firewall-apk-audit.log
docker exec "${container_name}" mv -f /tmp/nftables.nft.before-audit-test /etc/nftables.nft

echo "[firewall-test] compact safe and custom nft terminals on one line must not be conflated"
docker exec "${container_name}" sh -ec '
  printf '\''%s\n'\'' '\''table inet filter { chain input { ct state invalid drop; ip saddr 203.0.113.0/24 drop; }; }'\'' \
    >/etc/nftables.d/10_pending_compact.nft
  nft -c -f /etc/nftables.nft
'
if docker exec "${container_name}" sh /workspace/scripts/install-alpine.sh \
  --archive "${archive_in_container}" \
  --checksum "${checksums_in_container}" \
  >/tmp/wwan-proxy-alpine-firewall-compact.log 2>&1; then
  echo "installer unexpectedly conflated safe and custom nft terminals on one line" >&2
  exit 1
fi
grep -Fq 'nftables fragment has terminal control flow' \
  /tmp/wwan-proxy-alpine-firewall-compact.log
docker exec "${container_name}" rm -f /etc/nftables.d/10_pending_compact.nft

echo "[firewall-test] a hash inside an nft quoted comment must not hide a later terminal"
docker exec "${container_name}" sh -ec '
  printf '\''%s\n'\'' '\''table inet filter { chain input { ct state invalid drop comment "foo#bar"; ip saddr 203.0.113.0/24 drop; }; }'\'' \
    >/etc/nftables.d/10_pending_hash.nft
  nft -c -f /etc/nftables.nft
'
if docker exec "${container_name}" sh /workspace/scripts/install-alpine.sh \
  --archive "${archive_in_container}" \
  --checksum "${checksums_in_container}" \
  >/tmp/wwan-proxy-alpine-firewall-hash.log 2>&1; then
  echo "installer unexpectedly treated a quoted hash as the start of an nft line comment" >&2
  exit 1
fi
grep -Fq 'nftables fragment has terminal control flow' \
  /tmp/wwan-proxy-alpine-firewall-hash.log
docker exec "${container_name}" rm -f /etc/nftables.d/10_pending_hash.nft

echo "[firewall-test] arbitrary nft quoted strings cannot bypass conservative fragment ownership"
docker exec "${container_name}" sh -ec '
  printf '\''%s\n'\'' '\''table inet filter { chain input { log prefix "comment "; ip saddr 203.0.113.0/24 drop; }; }'\'' \
    >/etc/nftables.d/10_pending_prefix.nft
  nft -c -f /etc/nftables.nft
'
if docker exec "${container_name}" sh /workspace/scripts/install-alpine.sh \
  --archive "${archive_in_container}" \
  --checksum "${checksums_in_container}" \
  >/tmp/wwan-proxy-alpine-firewall-prefix.log 2>&1; then
  echo "installer unexpectedly trusted an unmanaged nftables startup fragment" >&2
  exit 1
fi
grep -Fq 'unmanaged nftables startup fragment' \
  /tmp/wwan-proxy-alpine-firewall-prefix.log
docker exec "${container_name}" rm -f /etc/nftables.d/10_pending_prefix.nft

echo "[firewall-test] custom terminal nftables flow must fail closed"
docker exec "${container_name}" nft 'add rule inet filter input drop comment "wwan-proxy CI custom terminal"'
if docker exec "${container_name}" sh /workspace/scripts/install-alpine.sh \
  --archive "${archive_in_container}" \
  --checksum "${checksums_in_container}" \
  >/tmp/wwan-proxy-alpine-firewall-custom.log 2>&1; then
  echo "installer unexpectedly claimed success for custom terminal nftables flow" >&2
  exit 1
fi
grep -Fq 'contains custom terminal control flow' /tmp/wwan-proxy-alpine-firewall-custom.log

echo "[firewall-test] enabled but stopped firewalld with an offline DROP zone must fail closed"
docker run --detach --privileged \
  --name "${firewalld_container_name}" \
  --volume "${repo_root}:/workspace:ro" \
  alpine:3.23 \
  sh -ec 'apk add --no-cache openrc firewalld >/tmp/bootstrap.log 2>&1; exec /sbin/init' \
  >/dev/null
for attempt in $(seq 1 30); do
  if docker exec "${firewalld_container_name}" rc-status --all >/dev/null 2>&1; then
    break
  fi
  if [[ ${attempt} -eq 30 ]]; then
    docker logs "${firewalld_container_name}" >&2 || true
    exit 1
  fi
  sleep 1
done
docker exec "${firewalld_container_name}" sh -ec '
  firewall-offline-cmd --set-default-zone=drop >/dev/null
  rc-update add firewalld default >/dev/null
'
if docker exec "${firewalld_container_name}" sh /workspace/scripts/install-alpine.sh \
  --archive "${archive_in_container}" \
  --checksum "${checksums_in_container}" \
  >/tmp/wwan-proxy-alpine-firewalld-stopped.log 2>&1; then
  echo "installer unexpectedly trusted enabled but stopped firewalld" >&2
  exit 1
fi
grep -Fq 'an OpenRC firewall service is enabled but not active' \
  /tmp/wwan-proxy-alpine-firewalld-stopped.log
firewalld_container_ipv4=$(docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' "${firewalld_container_name}")
curl --noproxy '*' --fail --silent --show-error --connect-timeout 2 --max-time 3 \
  "http://${firewalld_container_ipv4}:9090/api/health" >/dev/null
docker exec "${firewalld_container_name}" rc-service firewalld start >/dev/null
for attempt in $(seq 1 30); do
  if docker exec "${firewalld_container_name}" firewall-cmd --state 2>/dev/null | grep -Fqx running; then
    break
  fi
  if [[ ${attempt} -eq 30 ]]; then
    docker exec "${firewalld_container_name}" rc-service firewalld status >&2 || true
    exit 1
  fi
  sleep 1
done
test "$(docker exec "${firewalld_container_name}" firewall-cmd --get-default-zone)" = drop
if curl --noproxy '*' --fail --silent --show-error --connect-timeout 2 --max-time 3 \
  "http://${firewalld_container_ipv4}:9090/api/health" >/dev/null 2>&1; then
  echo "firewalld offline DROP zone unexpectedly allowed WebUI after start" >&2
  exit 1
fi

echo "[firewall-test] active firewalld and nftables owners must fail before either is mutated"
docker exec "${firewalld_container_name}" sh -ec '
  rc-service firewalld stop >/dev/null
  rc-update add nftables default >/dev/null
  rc-service nftables start >/dev/null
  rc-service firewalld start >/dev/null
'
for attempt in $(seq 1 30); do
  if docker exec "${firewalld_container_name}" firewall-cmd --state 2>/dev/null | grep -Fqx running; then
    break
  fi
  if [[ ${attempt} -eq 30 ]]; then
    docker exec "${firewalld_container_name}" rc-service firewalld status >&2 || true
    exit 1
  fi
  sleep 1
done
docker exec "${firewalld_container_name}" rc-service nftables status >/dev/null
if docker exec "${firewalld_container_name}" sh /workspace/scripts/install-alpine.sh \
  --archive "${archive_in_container}" \
  --checksum "${checksums_in_container}" \
  >/tmp/wwan-proxy-alpine-firewall-dual-owner.log 2>&1; then
  echo "installer unexpectedly trusted simultaneous firewalld and nftables owners" >&2
  exit 1
fi
grep -Fq 'multiple independent firewall owners are active' \
  /tmp/wwan-proxy-alpine-firewall-dual-owner.log
firewalld_default_zone=$(docker exec "${firewalld_container_name}" firewall-cmd --get-default-zone)
if docker exec "${firewalld_container_name}" firewall-cmd \
  --zone="${firewalld_default_zone}" --query-port=9090/tcp >/dev/null 2>&1; then
  echo "installer mutated firewalld before rejecting the dual-owner ruleset" >&2
  exit 1
fi
if curl --noproxy '*' --fail --silent --show-error --connect-timeout 2 --max-time 3 \
  "http://${firewalld_container_ipv4}:9090/api/health" >/dev/null 2>&1; then
  echo "dual-owner DROP ruleset unexpectedly allowed WebUI" >&2
  exit 1
fi

echo "[firewall-test] live native nft hooks conflict even when their OpenRC service is disabled"
docker exec "${firewalld_container_name}" sh -ec '
  rc-service firewalld stop >/dev/null
  rc-service nftables stop >/dev/null
  rc-update del nftables default >/dev/null
  nft -f /etc/nftables.nft
  rc-service firewalld start >/dev/null
'
for attempt in $(seq 1 30); do
  if docker exec "${firewalld_container_name}" firewall-cmd --state 2>/dev/null | grep -Fqx running; then
    break
  fi
  if [[ ${attempt} -eq 30 ]]; then
    docker exec "${firewalld_container_name}" rc-service firewalld status >&2 || true
    exit 1
  fi
  sleep 1
done
if docker exec "${firewalld_container_name}" rc-service nftables status >/dev/null 2>&1; then
  echo "nftables OpenRC unexpectedly remained started in the manual-hook test" >&2
  exit 1
fi
docker exec "${firewalld_container_name}" nft list chain inet filter input >/dev/null
if docker exec "${firewalld_container_name}" sh /workspace/scripts/install-alpine.sh \
  --archive "${archive_in_container}" \
  --checksum "${checksums_in_container}" \
  >/tmp/wwan-proxy-alpine-firewall-manual-hook.log 2>&1; then
  echo "installer unexpectedly trusted a manually loaded native nft INPUT hook" >&2
  exit 1
fi
grep -Fq 'multiple independent firewall owners are active' \
  /tmp/wwan-proxy-alpine-firewall-manual-hook.log
firewalld_default_zone=$(docker exec "${firewalld_container_name}" firewall-cmd --get-default-zone)
if docker exec "${firewalld_container_name}" firewall-cmd \
  --zone="${firewalld_default_zone}" --query-port=9090/tcp >/dev/null 2>&1; then
  echo "installer mutated firewalld before rejecting the manual native nft hook" >&2
  exit 1
fi
if curl --noproxy '*' --fail --silent --show-error --connect-timeout 2 --max-time 3 \
  "http://${firewalld_container_ipv4}:9090/api/health" >/dev/null 2>&1; then
  echo "manual native nft DROP hook unexpectedly allowed WebUI" >&2
  exit 1
fi

echo "[firewall-test] UFW must reject an additional custom-name nft INPUT hook"
docker run --detach --privileged \
  --name "${ufw_container_name}" \
  --volume "${repo_root}:/workspace:ro" \
  alpine:3.23 \
  sh -ec '
    for apk_attempt in $(seq 1 5); do
      if apk add --no-cache openrc ufw nftables >/tmp/bootstrap.log 2>&1; then
        break
      fi
      echo "apk attempt ${apk_attempt} failed, retrying..." >>/tmp/bootstrap.log
      sleep 3
    done
    # Alpine package extraction is normally synchronous, but CI overlayfs mirrors
    # occasionally leave the ufw binary invisible for a short moment; give it a
    # chance to settle before failing the whole job.
    for verify_attempt in $(seq 1 15); do
      if test -x /usr/sbin/ufw; then
        break
      fi
      sleep 1
    done
    if ! test -x /usr/sbin/ufw; then
      echo "ufw package did not install /usr/sbin/ufw" >&2
      cat /tmp/bootstrap.log >&2
      exit 1
    fi
    exec /sbin/init
  ' \
  >/dev/null
for attempt in $(seq 1 30); do
  if docker exec "${ufw_container_name}" rc-status --all >/dev/null 2>&1; then
    break
  fi
  if [[ ${attempt} -eq 30 ]]; then
    docker logs "${ufw_container_name}" >&2 || true
    exit 1
  fi
  sleep 1
done
if ! docker exec "${ufw_container_name}" test -x /usr/sbin/ufw; then
  echo "/usr/sbin/ufw not found after bootstrap" >&2
  docker exec "${ufw_container_name}" cat /tmp/bootstrap.log >&2 || true
  exit 1
fi
docker exec "${ufw_container_name}" sh -ec '
  /usr/sbin/ufw --force enable >/dev/null
  rc-update add ufw default >/dev/null
  nft add table inet guard
  nft "add chain inet guard input { type filter hook input priority -5; policy accept; }"
  nft '\''add rule inet guard input iifname "lo" accept'\''
  nft '\''add rule inet guard input ct state established,related accept'\''
  nft add rule inet guard input drop
'
docker exec "${ufw_container_name}" sh -ec '/usr/sbin/ufw status | grep -Eq "^Status:[[:space:]]+active$"'
if docker exec "${ufw_container_name}" sh /workspace/scripts/install-alpine.sh \
  --archive "${archive_in_container}" \
  --checksum "${checksums_in_container}" \
  >/tmp/wwan-proxy-alpine-firewall-ufw-extra-hook.log 2>&1; then
  echo "installer unexpectedly trusted UFW alongside an extra nft INPUT hook" >&2
  exit 1
fi
grep -Fq 'multiple independent firewall owners are active' \
  /tmp/wwan-proxy-alpine-firewall-ufw-extra-hook.log
if docker exec "${ufw_container_name}" /usr/sbin/ufw status | grep -Eq '(^|[[:space:]])9090(/tcp)?([[:space:]]|$)'; then
  echo "installer mutated UFW before rejecting the extra INPUT hook" >&2
  exit 1
fi
ufw_container_ipv4=$(docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' "${ufw_container_name}")
if curl --noproxy '*' --fail --silent --show-error --connect-timeout 2 --max-time 3 \
  "http://${ufw_container_ipv4}:9090/api/health" >/dev/null 2>&1; then
  echo "UFW plus custom nft DROP hook unexpectedly allowed WebUI" >&2
  exit 1
fi

echo "[firewall-test] starting Alpine 3.23 iptables-nft ownership/idempotence checks"
docker run --detach --privileged \
  --name "${iptables_container_name}" \
  --volume "${repo_root}:/workspace:ro" \
  alpine:3.23 \
  sh -ec 'apk add --no-cache openrc iptables >/tmp/bootstrap.log 2>&1; exec /sbin/init' \
  >/dev/null
for attempt in $(seq 1 30); do
  if docker exec "${iptables_container_name}" rc-status --all >/dev/null 2>&1; then
    break
  fi
  if [[ ${attempt} -eq 30 ]]; then
    docker logs "${iptables_container_name}" >&2 || true
    exit 1
  fi
  sleep 1
done
docker exec "${iptables_container_name}" sh -ec '
  iptables -F
  iptables -P INPUT DROP
  iptables -P FORWARD DROP
  iptables -P OUTPUT ACCEPT
  iptables -A INPUT -i lo -j ACCEPT
  iptables -A INPUT -m conntrack --ctstate ESTABLISHED,RELATED -j ACCEPT
  rc-service iptables save >/dev/null
  rc-service iptables start >/dev/null
  rc-update add iptables default >/dev/null
  iptables -A INPUT -p udp --dport 19003 -m comment --comment "-p tcp --dport 9090 -j ACCEPT" -j ACCEPT
'
docker exec "${iptables_container_name}" sh /workspace/scripts/install-alpine.sh \
  --archive "${archive_in_container}" \
  --checksum "${checksums_in_container}" \
  >/tmp/wwan-proxy-alpine-iptables-open.log
iptables_container_ipv4=$(docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' "${iptables_container_name}")
curl --noproxy '*' --fail --silent --show-error --connect-timeout 2 --max-time 3 \
  "http://${iptables_container_ipv4}:9090/api/health" >/dev/null
test "$(docker exec "${iptables_container_name}" iptables -S INPUT | grep -c 'wwan-proxy WebUI')" -eq 1
test "$(docker exec "${iptables_container_name}" grep -c 'wwan-proxy WebUI' /etc/iptables/rules-save)" -eq 1
docker exec "${iptables_container_name}" sh -ec '
  awk '\''
    $0 == "*filter" { in_filter = 1; next }
    in_filter && ($1 == "-A" || ($1 ~ /^\[[0-9]+:[0-9]+\]$/ && $2 == "-A")) {
      if ($0 !~ /wwan-proxy WebUI/) exit 1
      found = 1
      exit 0
    }
    END { if (!found) exit 1 }
  '\'' /etc/iptables/rules-save
  rc-service iptables restart >/dev/null
'
curl --noproxy '*' --fail --silent --show-error --connect-timeout 2 --max-time 3 \
  "http://${iptables_container_ipv4}:9090/api/health" >/dev/null

echo "[firewall-test] UDP allow must not be mistaken for TCP and runtime-only rules must not be persisted"
docker exec "${iptables_container_name}" sh -ec '
  iptables -D INPUT 1
  iptables -A INPUT -p udp --dport 9090 -m comment --comment "wwan-proxy UDP-only regression" -j ACCEPT
  iptables -A INPUT -p udp --dport 19002 -m comment --comment "wwan-proxy runtime-only regression" -j ACCEPT
'
docker exec "${iptables_container_name}" sh /workspace/scripts/install-alpine.sh \
  --archive "${archive_in_container}" \
  --checksum "${checksums_in_container}" \
  >/tmp/wwan-proxy-alpine-iptables-repeat.log
docker exec "${iptables_container_name}" iptables -C INPUT -p tcp --dport 9090 \
  -m comment --comment 'wwan-proxy WebUI' -j ACCEPT
docker exec "${iptables_container_name}" iptables -C INPUT -p udp --dport 9090 \
  -m comment --comment 'wwan-proxy UDP-only regression' -j ACCEPT
test "$(docker exec "${iptables_container_name}" iptables -S INPUT | grep -c 'wwan-proxy WebUI')" -eq 1
if docker exec "${iptables_container_name}" grep -Fq 'wwan-proxy runtime-only regression' /etc/iptables/rules-save; then
  echo "iptables runtime-only rule was unexpectedly persisted" >&2
  exit 1
fi

echo "[firewall-test] an earlier custom iptables deny must not be bypassed"
docker exec "${iptables_container_name}" iptables -I INPUT 1 -s 203.0.113.0/24 -j DROP
if docker exec "${iptables_container_name}" sh /workspace/scripts/install-alpine.sh \
  --archive "${archive_in_container}" \
  --checksum "${checksums_in_container}" \
  >/tmp/wwan-proxy-alpine-iptables-custom.log 2>&1; then
  echo "installer unexpectedly inserted an allow ahead of a custom iptables deny" >&2
  exit 1
fi
grep -Fq 'contains a custom deny or control-flow rule' /tmp/wwan-proxy-alpine-iptables-custom.log
test "$(docker exec "${iptables_container_name}" iptables -S INPUT | grep -c 'wwan-proxy WebUI')" -eq 1

echo "[firewall-test] pending persistent iptables deny must fail before restart"
docker exec "${iptables_container_name}" sh -ec '
  iptables -D INPUT -s 203.0.113.0/24 -j DROP
  awk '\''
    $0 == "*filter" { in_filter = 1 }
    in_filter && /wwan-proxy WebUI/ { next }
    in_filter && $0 == "COMMIT" {
      print "[0:0] -A INPUT -s 198.51.100.0/24 -j DROP"
      in_filter = 0
    }
    { print }
  '\'' /etc/iptables/rules-save >/tmp/rules-save.pending
  mv -f /tmp/rules-save.pending /etc/iptables/rules-save
  iptables-restore --test </etc/iptables/rules-save
'
if docker exec "${iptables_container_name}" sh /workspace/scripts/install-alpine.sh \
  --archive "${archive_in_container}" \
  --checksum "${checksums_in_container}" \
  >/tmp/wwan-proxy-alpine-iptables-pending.log 2>&1; then
  echo "installer unexpectedly accepted a pending persistent iptables deny" >&2
  exit 1
fi
grep -Fq 'contains a persistent INPUT custom deny or control-flow rule' \
  /tmp/wwan-proxy-alpine-iptables-pending.log
test "$(docker exec "${iptables_container_name}" iptables -S INPUT | grep -c 'wwan-proxy WebUI')" -eq 1

echo "[firewall-test] Alpine 3.23 nftables and iptables-nft checks passed"

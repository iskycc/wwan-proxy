#!/bin/sh

# Install Dante on Alpine Linux for a cloud-NAT host where the public IPv4
# address is not assigned directly to the external interface.

set -eu

PROGRAM_NAME="install-dante-alpine.sh"
SUPPORTED_ALPINE="3.21 3.22 3.23"

PUBLIC_IP=${DANTE_PUBLIC_IP:-}
PRIVATE_IP=${DANTE_PRIVATE_IP:-}
SOCKS_PORT=${DANTE_SOCKS_PORT:-18125}
UDP_RANGE=${DANTE_UDP_RANGE:-16000:17000}
EXTERNAL_IFACE=${DANTE_EXTERNAL_IFACE:-eth0}
SOCKS_USER=${DANTE_USER:-}
SOCKS_PASSWORD=${DANTE_PASSWORD:-}
ENABLE_SNAT=${DANTE_ENABLE_SNAT:-0}
NO_START=${DANTE_NO_START:-0}
FORCE_OS=${DANTE_FORCE_OS:-0}

SOCKD_CONFIG=/etc/sockd.conf
SOCKD_CONFD=/etc/conf.d/sockd
CLOUD_CONFIG=/etc/conf.d/dante-cloud
ALIAS_INIT=/etc/init.d/dante-public-alias
NAT_INIT=/etc/init.d/dante-cloud-nat
BACKUP_ROOT=/var/backups/dante-cloud
INSTALL_LOG=/var/log/dante-cloud-install.log

usage() {
	cat <<'EOF'
Usage:
  install-dante-alpine.sh --public-ip IP --private-ip IP --user USER [options]

Install and configure Dante on Alpine 3.21-3.23 for a cloud-NAT host. The
public IPv4 address is installed as a /32 alias on loopback. Traffic delivered
by the cloud to the private IPv4 address is DNATed to that public alias.

Required options:
  --public-ip IP          Public IPv4 address returned by Dante to clients
  --private-ip IP         Private IPv4 address assigned to the cloud instance
  --user USER             System user allowed to authenticate to SOCKS5

Other options:
  --port PORT             SOCKS5 TCP port (default: 18125)
  --udp-range A:B         Dante client-side UDP relay range (default: 16000:17000)
  --external-iface IFACE  Outbound interface (default: eth0)
  --enable-snat           SNAT packets sourced from the public alias to PRIVATE_IP
  --no-start              Install and validate files without enabling services
  --force-os              Allow Alpine versions outside 3.21-3.23
  -h, --help              Show this help

Environment equivalents:
  DANTE_PUBLIC_IP, DANTE_PRIVATE_IP, DANTE_SOCKS_PORT, DANTE_UDP_RANGE,
  DANTE_EXTERNAL_IFACE, DANTE_USER, DANTE_PASSWORD, DANTE_ENABLE_SNAT,
  DANTE_NO_START, DANTE_FORCE_OS

For security, the password is not accepted as a command-line option. Set
DANTE_PASSWORD or run from a terminal and passwd(1) will prompt when creating
the user. Re-running without DANTE_PASSWORD preserves an existing password.

Example:
  DANTE_PASSWORD='change-me' sh install-dante-alpine.sh \
    --public-ip 203.0.113.10 --private-ip 172.16.0.4 --user proxy_user
EOF
}

fatal() {
	printf '%s: %s\n' "${PROGRAM_NAME}" "$*" >&2
	exit 1
}

log() {
	message=$*
	printf '%s\n' "${message}"
	printf '%s %s\n' "$(date -u '+%Y-%m-%dT%H:%M:%SZ')" "${message}" >>"${INSTALL_LOG}" 2>/dev/null || true
}

is_true() {
	case "$1" in
		1|true|TRUE|yes|YES|on|ON) return 0 ;;
		*) return 1 ;;
	esac
}

is_ipv4() {
	address=$1
	case "${address}" in
		''|*[!0-9.]*) return 1 ;;
	esac
	octet1=${address%%.*}
	address_tail=${address#*.}
	[ "${address_tail}" != "${address}" ] || return 1
	octet2=${address_tail%%.*}
	address_tail_next=${address_tail#*.}
	[ "${address_tail_next}" != "${address_tail}" ] || return 1
	octet3=${address_tail_next%%.*}
	octet4=${address_tail_next#*.}
	[ "${octet4}" != "${address_tail_next}" ] || return 1
	case "${octet4}" in
		*.*) return 1 ;;
	esac
	for octet in "${octet1}" "${octet2}" "${octet3}" "${octet4}"; do
		[ -n "${octet}" ] || return 1
		[ "${octet}" -ge 0 ] 2>/dev/null || return 1
		[ "${octet}" -le 255 ] 2>/dev/null || return 1
	done
	return 0
}

validate_port() {
	port_name=$1
	port_value=$2
	case "${port_value}" in
		''|*[!0-9]*) fatal "${port_name} must be an integer: ${port_value}" ;;
	esac
	[ "${port_value}" -ge 1 ] && [ "${port_value}" -le 65535 ] || \
		fatal "${port_name} must be between 1 and 65535: ${port_value}"
}

while [ "$#" -gt 0 ]; do
	case "$1" in
		--public-ip)
			[ "$#" -ge 2 ] || fatal "--public-ip requires a value"
			PUBLIC_IP=$2
			shift 2
			;;
		--private-ip)
			[ "$#" -ge 2 ] || fatal "--private-ip requires a value"
			PRIVATE_IP=$2
			shift 2
			;;
		--port)
			[ "$#" -ge 2 ] || fatal "--port requires a value"
			SOCKS_PORT=$2
			shift 2
			;;
		--udp-range)
			[ "$#" -ge 2 ] || fatal "--udp-range requires a value"
			UDP_RANGE=$2
			shift 2
			;;
		--external-iface)
			[ "$#" -ge 2 ] || fatal "--external-iface requires a value"
			EXTERNAL_IFACE=$2
			shift 2
			;;
		--user)
			[ "$#" -ge 2 ] || fatal "--user requires a value"
			SOCKS_USER=$2
			shift 2
			;;
		--enable-snat)
			ENABLE_SNAT=1
			shift
			;;
		--no-start)
			NO_START=1
			shift
			;;
		--force-os)
			FORCE_OS=1
			shift
			;;
		-h|--help)
			usage
			exit 0
			;;
		--)
			shift
			break
			;;
		*)
			fatal "unknown option: $1"
			;;
	esac
done

[ "$#" -eq 0 ] || fatal "unexpected positional arguments: $*"
[ "$(id -u)" -eq 0 ] || fatal "run this installer as root"
[ -r /etc/alpine-release ] || fatal "/etc/alpine-release is missing; this installer requires Alpine Linux"

if is_true "${ENABLE_SNAT}"; then
	ENABLE_SNAT=1
else
	ENABLE_SNAT=0
fi
if is_true "${NO_START}"; then
	NO_START=1
else
	NO_START=0
fi

ALPINE_RELEASE=$(cat /etc/alpine-release)
release_supported=0
for supported_release in ${SUPPORTED_ALPINE}; do
	case "${ALPINE_RELEASE}" in
		${supported_release}.*) release_supported=1 ;;
	esac
done
if [ "${release_supported}" -ne 1 ] && ! is_true "${FORCE_OS}"; then
	fatal "Alpine ${ALPINE_RELEASE} is unsupported; expected 3.21.x through 3.23.x"
fi

[ -n "${PUBLIC_IP}" ] || fatal "--public-ip or DANTE_PUBLIC_IP is required"
[ -n "${PRIVATE_IP}" ] || fatal "--private-ip or DANTE_PRIVATE_IP is required"
[ -n "${SOCKS_USER}" ] || fatal "--user or DANTE_USER is required"
is_ipv4 "${PUBLIC_IP}" || fatal "invalid public IPv4 address: ${PUBLIC_IP}"
is_ipv4 "${PRIVATE_IP}" || fatal "invalid private IPv4 address: ${PRIVATE_IP}"
[ "${PUBLIC_IP}" != 0.0.0.0 ] && [ "${PUBLIC_IP}" != 255.255.255.255 ] || fatal "invalid public IPv4 address: ${PUBLIC_IP}"
[ "${PRIVATE_IP}" != 0.0.0.0 ] && [ "${PRIVATE_IP}" != 255.255.255.255 ] || fatal "invalid private IPv4 address: ${PRIVATE_IP}"
[ "${PUBLIC_IP}" != "${PRIVATE_IP}" ] || fatal "public and private IPv4 addresses must differ"

case "${EXTERNAL_IFACE}" in
	''|*[!A-Za-z0-9_.:@-]*) fatal "invalid external interface name: ${EXTERNAL_IFACE}" ;;
esac
case "${SOCKS_USER}" in
	''|*[!A-Za-z0-9_-]*|[!A-Za-z_]*) fatal "invalid SOCKS username: ${SOCKS_USER}" ;;
esac
case "${SOCKS_PASSWORD}" in
	*:*) fatal "DANTE_PASSWORD must not contain a colon" ;;
esac
newline='
'
case "${SOCKS_PASSWORD}" in
	*"${newline}"*) fatal "DANTE_PASSWORD must not contain a newline" ;;
esac

validate_port "SOCKS port" "${SOCKS_PORT}"
case "${UDP_RANGE}" in
	*:*:*) fatal "UDP range must use START:END syntax: ${UDP_RANGE}" ;;
	*:*) ;;
	*) fatal "UDP range must use START:END syntax: ${UDP_RANGE}" ;;
esac
UDP_START=${UDP_RANGE%%:*}
UDP_END=${UDP_RANGE#*:}
validate_port "UDP range start" "${UDP_START}"
validate_port "UDP range end" "${UDP_END}"
[ "${UDP_START}" -le "${UDP_END}" ] || fatal "UDP range start must not exceed its end"
UDP_RANGE_DANTE="${UDP_START}-${UDP_END}"

mkdir -p "$(dirname "${INSTALL_LOG}")"
touch "${INSTALL_LOG}"
chmod 0600 "${INSTALL_LOG}"

log "Installing Dante dependencies on Alpine ${ALPINE_RELEASE}"
apk add --no-cache dante-server iproute2 iptables openrc
if [ ! -x /etc/init.d/sockd ]; then
	apk add --no-cache dante-server-openrc
fi
[ -x /etc/init.d/sockd ] || fatal "the Dante package did not install /etc/init.d/sockd"

ip link show dev "${EXTERNAL_IFACE}" >/dev/null 2>&1 || fatal "interface does not exist: ${EXTERNAL_IFACE}"
ip -o -4 addr show dev "${EXTERNAL_IFACE}" | grep -Fq " ${PRIVATE_IP}/" || \
	fatal "${PRIVATE_IP} is not assigned to ${EXTERNAL_IFACE}; refusing to install DNAT rules"
public_interfaces=$(ip -o -4 addr show | awk -v ip="${PUBLIC_IP}" '{ split($4, address, "/"); if (address[1] == ip) print $2 }')
for public_interface in ${public_interfaces}; do
	[ "${public_interface}" = lo ] || fatal "${PUBLIC_IP} is already assigned to ${public_interface}; a loopback alias is not appropriate"
done

backup_stamp=$(date -u '+%Y%m%dT%H%M%SZ')
BACKUP_DIR="${BACKUP_ROOT}/${backup_stamp}"
mkdir -p "${BACKUP_DIR}"
for backup_file in \
	"${SOCKD_CONFIG}" "${SOCKD_CONFD}" "${CLOUD_CONFIG}" \
	"${ALIAS_INIT}" "${NAT_INIT}"
do
	if [ -e "${backup_file}" ]; then
		cp -p "${backup_file}" "${BACKUP_DIR}/$(basename "${backup_file}")"
	fi
done

if ! id "${SOCKS_USER}" >/dev/null 2>&1; then
	log "Creating SOCKS authentication user ${SOCKS_USER}"
	adduser -D -H -s /sbin/nologin "${SOCKS_USER}"
	user_created=1
else
	user_created=0
fi

if [ -n "${SOCKS_PASSWORD}" ]; then
	printf '%s:%s\n' "${SOCKS_USER}" "${SOCKS_PASSWORD}" | chpasswd
	log "Updated password for ${SOCKS_USER} from DANTE_PASSWORD"
elif [ "${user_created}" -eq 1 ]; then
	if [ -r /dev/tty ] && [ -w /dev/tty ]; then
		log "Set the password for ${SOCKS_USER}"
		passwd "${SOCKS_USER}" </dev/tty >/dev/tty
	else
		fatal "created ${SOCKS_USER}, but no terminal is available; set DANTE_PASSWORD and re-run"
	fi
else
	log "Preserving the existing password for ${SOCKS_USER}"
fi

# Stop the old managed stack before replacing its configuration. This makes
# re-runs with a different public address remove the previous alias and rules.
if [ -x /etc/init.d/sockd ]; then
	rc-service sockd stop >/dev/null 2>&1 || true
fi
if [ -x "${NAT_INIT}" ]; then
	rc-service dante-cloud-nat stop >/dev/null 2>&1 || true
fi
if [ -x "${ALIAS_INIT}" ]; then
	rc-service dante-public-alias stop >/dev/null 2>&1 || true
fi

umask 077
cloud_stage=$(mktemp /tmp/dante-cloud.conf.XXXXXX)
sockd_stage=$(mktemp /tmp/sockd.conf.XXXXXX)
alias_stage=$(mktemp /tmp/dante-public-alias.XXXXXX)
nat_stage=$(mktemp /tmp/dante-cloud-nat.XXXXXX)
sockd_confd_stage=$(mktemp /tmp/dante-sockd-confd.XXXXXX)
cleanup() {
	rm -f "${cloud_stage}" "${sockd_stage}" "${alias_stage}" "${nat_stage}" "${sockd_confd_stage}"
}
trap cleanup EXIT
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM

cat >"${cloud_stage}" <<EOF
# Managed by ${PROGRAM_NAME}. Re-run the installer to change these values.
PUBLIC_IP="${PUBLIC_IP}"
PRIVATE_IP="${PRIVATE_IP}"
SOCKS_PORT="${SOCKS_PORT}"
UDP_START="${UDP_START}"
UDP_END="${UDP_END}"
EXTERNAL_IFACE="${EXTERNAL_IFACE}"
ALIAS_DEVICE="lo"
ENABLE_SNAT="${ENABLE_SNAT}"
EOF

cat >"${sockd_stage}" <<EOF
# Managed by ${PROGRAM_NAME}.
logoutput: syslog

# The public address is installed as a /32 loopback alias. Dante returns this
# address to SOCKS5 UDP clients instead of the cloud-only private address.
internal: ${PUBLIC_IP} port = ${SOCKS_PORT}
external: ${EXTERNAL_IFACE}

clientmethod: none
socksmethod: username none

user.privileged: root
user.unprivileged: sockd

client pass {
	from: 0.0.0.0/0 to: 0.0.0.0/0
	log: connect disconnect error
}

socks pass {
	from: 0.0.0.0/0 to: 0.0.0.0/0
	command: connect udpassociate
	socksmethod: username
	user: ${SOCKS_USER}
	udp.portrange: ${UDP_RANGE_DANTE}
	log: connect disconnect error
}

socks pass {
	from: 0.0.0.0/0 to: 0.0.0.0/0
	command: udpreply
	socksmethod: none
	log: error
}
EOF

cat >"${sockd_confd_stage}" <<'EOF'
# Managed by install-dante-alpine.sh.
SOCKD_OPTS="-D"
rc_ulimit="-n 65536 -u unlimited"
rc_need="dante-public-alias dante-cloud-nat"
EOF

cat >"${alias_stage}" <<'EOF'
#!/sbin/openrc-run

description="Dante public IPv4 loopback alias"

depend() {
	need net
	before dante-cloud-nat sockd
}

load_config() {
	[ -r /etc/conf.d/dante-cloud ] || return 1
	. /etc/conf.d/dante-cloud
}

alias_exists() {
	ip -o -4 addr show dev "${ALIAS_DEVICE}" | grep -Fq " ${PUBLIC_IP}/32 "
}

start() {
	load_config || return 1
	ebegin "Adding Dante public alias ${PUBLIC_IP}/32 to ${ALIAS_DEVICE}"
	{
		ip link set dev "${ALIAS_DEVICE}" up
		alias_exists || ip addr add "${PUBLIC_IP}/32" dev "${ALIAS_DEVICE}"
	}
	eend $?
}

stop() {
	load_config || return 1
	ebegin "Removing Dante public alias ${PUBLIC_IP}/32 from ${ALIAS_DEVICE}"
	{
		alias_exists && ip addr del "${PUBLIC_IP}/32" dev "${ALIAS_DEVICE}" || true
	}
	eend $?
}
EOF

cat >"${nat_stage}" <<'EOF'
#!/sbin/openrc-run

description="Dante cloud NAT rules"

depend() {
	need net dante-public-alias
	after firewall iptables nftables
	before sockd
}

load_config() {
	[ -r /etc/conf.d/dante-cloud ] || return 1
	. /etc/conf.d/dante-cloud
}

ensure_rule() {
	table=$1
	chain=$2
	shift 2
	iptables -w 5 -t "${table}" -C "${chain}" "$@" 2>/dev/null || \
		iptables -w 5 -t "${table}" -A "${chain}" "$@"
}

remove_rule() {
	table=$1
	chain=$2
	shift 2
	while iptables -w 5 -t "${table}" -C "${chain}" "$@" 2>/dev/null; do
		iptables -w 5 -t "${table}" -D "${chain}" "$@" || return 1
	done
}

start() {
	ebegin "Installing Dante cloud NAT rules"
	load_config && \
	ensure_rule nat PREROUTING \
		-d "${PRIVATE_IP}/32" -p tcp --dport "${SOCKS_PORT}" \
		-m comment --comment "dante-cloud tcp" \
		-j DNAT --to-destination "${PUBLIC_IP}:${SOCKS_PORT}" && \
	ensure_rule nat PREROUTING \
		-d "${PRIVATE_IP}/32" -p udp --dport "${UDP_START}:${UDP_END}" \
		-m comment --comment "dante-cloud udp" \
		-j DNAT --to-destination "${PUBLIC_IP}"
	status=$?
	if [ "${status}" -eq 0 ] && [ "${ENABLE_SNAT}" = 1 ]; then
		ensure_rule nat POSTROUTING \
			-s "${PUBLIC_IP}/32" -o "${EXTERNAL_IFACE}" \
			-m comment --comment "dante-cloud snat" \
			-j SNAT --to-source "${PRIVATE_IP}"
		status=$?
	fi
	eend "${status}"
}

stop() {
	ebegin "Removing Dante cloud NAT rules"
	load_config || { eend 1; return 1; }
	remove_rule nat PREROUTING \
		-d "${PRIVATE_IP}/32" -p tcp --dport "${SOCKS_PORT}" \
		-m comment --comment "dante-cloud tcp" \
		-j DNAT --to-destination "${PUBLIC_IP}:${SOCKS_PORT}"
	remove_rule nat PREROUTING \
		-d "${PRIVATE_IP}/32" -p udp --dport "${UDP_START}:${UDP_END}" \
		-m comment --comment "dante-cloud udp" \
		-j DNAT --to-destination "${PUBLIC_IP}"
	remove_rule nat POSTROUTING \
		-s "${PUBLIC_IP}/32" -o "${EXTERNAL_IFACE}" \
		-m comment --comment "dante-cloud snat" \
		-j SNAT --to-source "${PRIVATE_IP}"
	eend $?
}
EOF

install -m 0644 "${sockd_stage}" "${SOCKD_CONFIG}"
install -m 0644 "${sockd_confd_stage}" "${SOCKD_CONFD}"
install -m 0600 "${cloud_stage}" "${CLOUD_CONFIG}"
install -m 0755 "${alias_stage}" "${ALIAS_INIT}"
install -m 0755 "${nat_stage}" "${NAT_INIT}"

log "Validating the generated OpenRC and Dante configuration"
sh -n "${ALIAS_INIT}"
sh -n "${NAT_INIT}"

# sockd -V validates interface addresses as well as syntax, so bring up the
# alias temporarily even when --no-start was selected.
rc-service dante-public-alias start
if ! sockd -V -f "${SOCKD_CONFIG}"; then
	rc-service dante-public-alias stop >/dev/null 2>&1 || true
	fatal "Dante rejected ${SOCKD_CONFIG}; backups are in ${BACKUP_DIR}"
fi

if is_true "${NO_START}"; then
	rc-service dante-public-alias stop >/dev/null 2>&1 || true
	rc-update del sockd default >/dev/null 2>&1 || true
	rc-update del dante-cloud-nat default >/dev/null 2>&1 || true
	rc-update del dante-public-alias default >/dev/null 2>&1 || true
	log "Installation validated; --no-start left all services disabled and stopped"
else
	rc-update add dante-public-alias default
	rc-update add dante-cloud-nat default
	rc-update add sockd default
	rc-service dante-cloud-nat start
	rc-service sockd start
	rc-service sockd status
	log "Dante is listening at ${PUBLIC_IP}:${SOCKS_PORT}; UDP relay range is ${UDP_START}:${UDP_END}"
fi

if is_true "${ENABLE_SNAT}"; then
	log "Optional SNAT is enabled: ${PUBLIC_IP} -> ${PRIVATE_IP} on ${EXTERNAL_IFACE}"
else
	log "Optional SNAT is disabled; re-run with --enable-snat only if external reply testing requires it"
fi
log "Configuration backup: ${BACKUP_DIR}"
log "Installation log: ${INSTALL_LOG}"

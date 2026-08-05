#!/bin/sh

# One-click installer for wwan-proxy on Alpine Linux 3.21-3.23.
# BusyBox ash compatible: intentionally avoids Bash-only syntax.
# ShellCheck cannot see trap callbacks and pgrep is not guaranteed in BusyBox.
# shellcheck disable=SC2009,SC2329

set -u

PROGRAM_NAME="install-alpine.sh"
SERVICE_NAME="wwan-proxy"
DEFAULT_REPOSITORY="iskycc/wwan-proxy"
SUPPORTED_ALPINE_SERIES="3.21 3.22 3.23"
SUPPORTED_ALPINE_DISPLAY="3.21.x through 3.23.x"
DEFAULT_HEALTH_URL="http://127.0.0.1:9090/api/health"
ALPINE_WEB_DEFAULT="0.0.0.0:9090"

INSTALL_BINARY="/usr/local/bin/wwan-proxy"
INSTALL_INIT="/etc/init.d/wwan-proxy"
INSTALL_CONF="/etc/conf.d/wwan-proxy"
INSTALL_LOGROTATE="/etc/logrotate.d/wwan-proxy"
DATA_DIR="/var/lib/wwan-proxy"
DATABASE_PATH="${DATA_DIR}/wwan-proxy.db"
SERVICE_LOG_DIR="/var/log/wwan-proxy"
SERVICE_LOG="${SERVICE_LOG_DIR}/service.log"
INSTALL_LOG="/var/log/wwan-proxy-install.log"
BACKUP_ROOT="/var/backups/wwan-proxy"
LOCK_DIR="/run/wwan-proxy-install.lock"

REPOSITORY="${WWAN_PROXY_REPOSITORY:-${DEFAULT_REPOSITORY}}"
RELEASE_VERSION="${WWAN_PROXY_VERSION:-latest}"
LOCAL_ARCHIVE="${WWAN_PROXY_ARCHIVE:-}"
LOCAL_CHECKSUMS="${WWAN_PROXY_CHECKSUMS:-}"
HEALTH_URL="${WWAN_PROXY_HEALTH_URL:-}"
HEALTH_URL_EXPLICIT=0
[ -n "${HEALTH_URL}" ] && HEALTH_URL_EXPLICIT=1
NO_START="${WWAN_PROXY_NO_START:-0}"
FORCE_OS="${WWAN_PROXY_FORCE_OS:-0}"
START_TIMEOUT="${WWAN_PROXY_START_TIMEOUT:-30}"
FIREWALL_MODE="${WWAN_PROXY_FIREWALL:-open}"

RUN_ID="$(date -u '+%Y%m%dT%H%M%SZ')-$$"
CURRENT_STEP="bootstrap"
STEP_NUMBER=0
WORK_DIR=""
STEP_OUTPUT=""
LOCK_HELD=0
SUCCESS=0
ROLLBACK_READY=0
ROLLBACK_ACTIVE=0
PREVIOUS_RUNNING=0
PREVIOUS_ENABLED=0
HAD_BINARY=0
HAD_INIT=0
HAD_CONF=0
HAD_LOGROTATE=0
HAD_DATABASE=0
OLD_BINARY_CAP=""
FILES_REQUIRE_RESTART=0
SERVICE_FILES_TRUSTED=0
SERVICE_LOG_TRUSTED=0
INSTALLED_VERSION="unknown"
BACKUP_DIR=""
BINARY_STAGE=""
INIT_STAGE=""
FIREWALL_TRANSACTION=""
FIREWALL_TRANSACTION_ACTIVE=0
FIREWALL_ZONE=""
FIREWALL_ADDED_RUNTIME=0
FIREWALL_ADDED_PERMANENT=0
UFW_RULE_ADDED=0
NFT_FRAGMENT=""
NFT_FRAGMENT_BACKUP=""
NFT_FRAGMENT_EXISTED=0
NFT_FRAGMENT_TOUCHED=0
NFT_PUBLISH_STAGE=""
NFT_RUNTIME_HANDLE=""
NFT_RUNTIME_ADDED=0
NFT_OLD_RUNTIME_HANDLE=""
NFT_OLD_RUNTIME_REMOVED=0
IPTABLES_RULE_ADDED=0
IPTABLES_RULES_FILE="/etc/iptables/rules-save"
IPTABLES_RULES_BACKUP=""
IPTABLES_RULES_EXISTED=0
IPTABLES_RULES_TOUCHED=0
IPTABLES_PUBLISH_STAGE=""
IPTABLES_OLD_PORTS_FILE=""
IPTABLES_REMOVED_PORTS_FILE=""

usage() {
	cat <<'EOF'
Usage:
  install-alpine.sh [options]

Install or upgrade wwan-proxy on Alpine Linux 3.21-3.23 using a verified musl
release package and an OpenRC service.

Options:
  --version TAG          Release tag to install (default: latest)
  --repo OWNER/REPO      GitHub repository (default: iskycc/wwan-proxy)
  --archive FILE         Install a local release archive
  --checksum FILE        SHA256SUMS for --archive (required with --archive)
  --health-url URL       Post-start health endpoint
  --start-timeout SEC    Start/health timeout, 5-300 seconds (default: 30)
  --open-firewall        Inspect the firewall and open the WebUI TCP port
                         when the active backend can be changed safely (default)
  --check-firewall       Inspect and report firewall state without changing it
  --skip-firewall        Skip host firewall inspection and changes
  --no-start             Install files without starting OpenRC; firewall work
                         is deferred until a later normal installer run
  --force-os             Allow an Alpine release outside 3.21-3.23
  -h, --help             Show this help

Environment equivalents:
  WWAN_PROXY_VERSION, WWAN_PROXY_REPOSITORY, WWAN_PROXY_ARCHIVE,
  WWAN_PROXY_CHECKSUMS, WWAN_PROXY_HEALTH_URL, WWAN_PROXY_START_TIMEOUT,
  WWAN_PROXY_NO_START, WWAN_PROXY_FORCE_OS,
  WWAN_PROXY_FIREWALL (open, check, or skip; default: open)

Examples:
  curl -fsSL https://raw.githubusercontent.com/iskycc/wwan-proxy/main/scripts/install-alpine.sh | sh
  sh install-alpine.sh --version build-0123456789ab
  sh install-alpine.sh --check-firewall
  sh install-alpine.sh --archive ./wwan-proxy-linux-amd64-musl.tar.gz --checksum ./SHA256SUMS

Persistent installer log:
  /var/log/wwan-proxy-install.log
EOF
}

while [ "$#" -gt 0 ]; do
	case "$1" in
		--version)
			[ "$#" -ge 2 ] || { echo "${PROGRAM_NAME}: --version requires a value" >&2; exit 2; }
			RELEASE_VERSION=$2
			shift 2
			;;
		--repo)
			[ "$#" -ge 2 ] || { echo "${PROGRAM_NAME}: --repo requires a value" >&2; exit 2; }
			REPOSITORY=$2
			shift 2
			;;
		--archive)
			[ "$#" -ge 2 ] || { echo "${PROGRAM_NAME}: --archive requires a value" >&2; exit 2; }
			LOCAL_ARCHIVE=$2
			shift 2
			;;
		--checksum|--checksums)
			[ "$#" -ge 2 ] || { echo "${PROGRAM_NAME}: --checksum requires a value" >&2; exit 2; }
			LOCAL_CHECKSUMS=$2
			shift 2
			;;
		--health-url)
			[ "$#" -ge 2 ] || { echo "${PROGRAM_NAME}: --health-url requires a value" >&2; exit 2; }
			HEALTH_URL=$2
			HEALTH_URL_EXPLICIT=1
			shift 2
			;;
		--start-timeout)
			[ "$#" -ge 2 ] || { echo "${PROGRAM_NAME}: --start-timeout requires a value" >&2; exit 2; }
			START_TIMEOUT=$2
			shift 2
			;;
		--open-firewall)
			FIREWALL_MODE=open
			shift
			;;
		--check-firewall)
			FIREWALL_MODE=check
			shift
			;;
		--skip-firewall)
			FIREWALL_MODE=skip
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
			echo "${PROGRAM_NAME}: unknown option: $1" >&2
			usage >&2
			exit 2
			;;
	esac
done

if [ "$#" -ne 0 ]; then
	echo "${PROGRAM_NAME}: unexpected positional arguments: $*" >&2
	exit 2
fi

timestamp() {
	date -u '+%Y-%m-%dT%H:%M:%SZ'
}

log_line() {
	log_level=$1
	shift
	log_message=$*
	formatted_line="$(timestamp) [${RUN_ID}] [${log_level}] [${CURRENT_STEP}] ${log_message}"
	printf '%s\n' "${formatted_line}"
	if [ -n "${INSTALL_LOG}" ] && [ -e "${INSTALL_LOG}" ]; then
		printf '%s\n' "${formatted_line}" >>"${INSTALL_LOG}" 2>/dev/null || true
	fi
}

append_output_to_log() {
	output_label=$1
	output_file=$2
	[ -s "${output_file}" ] || return 0
	while IFS= read -r output_line || [ -n "${output_line}" ]; do
		printf '%s [%s] [OUTPUT] [%s] %s\n' "$(timestamp)" "${RUN_ID}" "${output_label}" "${output_line}" >>"${INSTALL_LOG}" 2>/dev/null || true
	done <"${output_file}"
}

command_text() {
	command_rendered=""
	for command_arg in "$@"; do
		case "${command_arg}" in
			*[!A-Za-z0-9_./:@%+=,-]*) command_arg="'$(printf '%s' "${command_arg}" | sed "s/'/'\\\\''/g")'" ;;
		esac
		if [ -z "${command_rendered}" ]; then
			command_rendered=${command_arg}
		else
			command_rendered="${command_rendered} ${command_arg}"
		fi
	done
	printf '%s' "${command_rendered}"
}

run_step() {
	step_description=$1
	shift
	STEP_NUMBER=$((STEP_NUMBER + 1))
	CURRENT_STEP="step-${STEP_NUMBER}"
	log_line INFO "BEGIN: ${step_description}"
	log_line DEBUG "command: $(command_text "$@")"
	if "$@" >>"${INSTALL_LOG}" 2>&1; then
		step_rc=0
	else
		step_rc=$?
	fi
	if [ "${step_rc}" -ne 0 ]; then
		log_line ERROR "FAILED (exit=${step_rc}): ${step_description}"
		printf '%s\n' "----- installer log (last 200 lines) -----" >&2
		tail -n 200 "${INSTALL_LOG}" >&2 || true
		printf '%s\n' "----- end installer log -----" >&2
		return "${step_rc}"
	fi
	log_line INFO "DONE (exit=0): ${step_description}"
	return 0
}

fatal() {
	log_line ERROR "$*"
	exit 1
}

assert_root_controlled_file() {
	trusted_path=$1
	trusted_label=$2
	[ -e "${trusted_path}" ] || return 0
	[ -f "${trusted_path}" ] || fatal "${trusted_label} is not a regular file: ${trusted_path}"
	trusted_metadata=$(stat -c '%u:%a' "${trusted_path}" 2>/dev/null || echo invalid)
	trusted_owner=${trusted_metadata%%:*}
	trusted_mode=${trusted_metadata#*:}
	case "${trusted_owner}" in
		0) ;;
		*) fatal "${trusted_label} must be owned by root before it can be trusted; found ${trusted_metadata}" ;;
	esac
	case "${trusted_mode}" in
		*[2367]) fatal "${trusted_label} is writable by other users; secure ${trusted_path} first (mode=${trusted_mode})" ;;
	esac
	case "${trusted_mode}" in
		*[2367][0-7]) fatal "${trusted_label} is group-writable; secure ${trusted_path} first (mode=${trusted_mode})" ;;
	esac
}

is_true() {
	case "$1" in
		1|yes|true|on|YES|TRUE|ON) return 0 ;;
		*) return 1 ;;
	esac
}

is_supported_alpine_release() {
	candidate_release=$1
	for supported_series in ${SUPPORTED_ALPINE_SERIES}; do
		case "${candidate_release}" in
			${supported_series}.*) return 0 ;;
		esac
	done
	return 1
}

is_root_safe_file() {
	safe_file_path=$1
	[ -f "${safe_file_path}" ] && [ ! -L "${safe_file_path}" ] || return 1
	safe_file_metadata=$(stat -c '%u:%a' "${safe_file_path}" 2>/dev/null || echo invalid)
	[ "${safe_file_metadata%%:*}" = "0" ] || return 1
	safe_file_mode=${safe_file_metadata#*:}
	case "${safe_file_mode}" in
		*[2367]|*[2367][0-7]) return 1 ;;
	esac
	return 0
}

is_root_safe_directory() {
	safe_dir_path=$1
	[ -d "${safe_dir_path}" ] && [ ! -L "${safe_dir_path}" ] || return 1
	safe_dir_metadata=$(stat -c '%u:%a' "${safe_dir_path}" 2>/dev/null || echo invalid)
	[ "${safe_dir_metadata%%:*}" = "0" ] || return 1
	safe_dir_mode=${safe_dir_metadata#*:}
	case "${safe_dir_mode}" in
		*[2367]|*[2367][0-7]) return 1 ;;
	esac
	return 0
}

read_effective_web_listener() {
	web_listener_error="${WORK_DIR}/web-listener-error"
	if ! WEB_LISTEN=$("${INSTALL_BINARY}" -db "${DATABASE_PATH}" -web-default "${ALPINE_WEB_DEFAULT}" -print-web-listen 2>"${web_listener_error}"); then
		append_output_to_log "read effective WebUI listener" "${web_listener_error}"
		return 1
	fi
	WEB_LISTEN=$(printf '%s' "${WEB_LISTEN}" | tail -n 1 | tr -d '\r')
	case "${WEB_LISTEN}" in
		\[*\]:*)
			WEB_HOST=${WEB_LISTEN#\[}
			WEB_HOST=${WEB_HOST%%\]*}
			WEB_PORT=${WEB_LISTEN##*:}
			;;
		*:*)
			WEB_HOST=${WEB_LISTEN%:*}
			WEB_PORT=${WEB_LISTEN##*:}
			;;
		*) return 1 ;;
	esac
	case "${WEB_PORT}" in
		*[!0-9]*|"") return 1 ;;
	esac
	[ "${WEB_PORT}" -ge 1 ] && [ "${WEB_PORT}" -le 65535 ] || return 1
	WEB_HOST_LOWER=$(printf '%s' "${WEB_HOST}" | tr '[:upper:]' '[:lower:]')
	return 0
}

is_ipv4_literal() {
	ipv4_value=$1
	printf '%s\n' "${ipv4_value}" | awk -F. '
		BEGIN { valid = 1 }
		NR != 1 || NF != 4 { valid = 0; exit }
		{
			for (i = 1; i <= 4; i++) {
				if ($i !~ /^[0-9]+$/ || $i + 0 > 255) {
					valid = 0
					exit
				}
			}
		}
		END { exit valid ? 0 : 1 }
	'
}

is_loopback_web_host() {
	case "${WEB_HOST_LOWER}" in
		localhost|::1) return 0 ;;
	esac
	if is_ipv4_literal "${WEB_HOST_LOWER}" && [ "${WEB_HOST_LOWER%%.*}" -eq 127 ]; then
		return 0
	fi
	return 1
}

sanitize_iptables_rules() {
	iptables_rules_source=$1
	iptables_rules_sanitized=$2
	awk '
		function ends_with_unescaped_quote(value, backslashes, position) {
			if (substr(value, length(value), 1) != "\"") return 0
			backslashes = 0
			for (position = length(value) - 1; position >= 1 && substr(value, position, 1) == "\\"; position--) backslashes++
			return backslashes % 2 == 0
		}
		{
			output = ""
			for (i = 1; i <= NF; i++) {
				if ($i == "-m" && i < NF && $(i + 1) == "comment") {
					i++
					continue
				}
				if ($i == "--comment") {
					if (i < NF) {
						i++
						if (substr($i, 1, 1) == "\"" && !ends_with_unescaped_quote($i)) {
							while (i < NF && !ends_with_unescaped_quote($i)) i++
						}
					}
					continue
				}
				output = output (output == "" ? "" : " ") $i
			}
			print output
		}
	' "${iptables_rules_source}" >"${iptables_rules_sanitized}"
}

extract_iptables_filter_input_rules() {
	iptables_rules_source=$1
	iptables_input_output=$2
	awk '
		/^\*/ {
			saw_tables = 1
			table_name = $1
			next
		}
		$1 == "COMMIT" {
			table_name = ""
			next
		}
		{
			if (saw_tables && table_name != "*filter") next
			rule_start = 0
			for (i = 1; i < NF; i++) {
				if (($i == "-A" || $i == "-P") && $(i + 1) == "INPUT") {
					rule_start = i
					break
				}
			}
			if (rule_start == 0) next
			output = ""
			for (i = rule_start; i <= NF; i++) output = output (output == "" ? "" : " ") $i
			print output
		}
	' "${iptables_rules_source}" >"${iptables_input_output}"
}

live_iptables_restricts_input() {
	live_iptables_file=$1
	command -v iptables-save >/dev/null 2>&1 || return 1
	iptables-save >"${live_iptables_file}" 2>>"${INSTALL_LOG}" || return 1
	live_iptables_sanitized="${live_iptables_file}.sanitized"
	sanitize_iptables_rules "${live_iptables_file}" "${live_iptables_sanitized}" || return 1
	grep -Eq '^:INPUT (DROP|REJECT) |(^|[[:space:]])-A INPUT .* -j (DROP|REJECT)([[:space:]]|$)' "${live_iptables_sanitized}"
}

iptables_chain_allows_tcp_port() {
	iptables_allow_file=$1
	iptables_allow_port=$2
	awk -v wanted_port="${iptables_allow_port}" '
		{
			protocol = ""
			destination_port = ""
			target = ""
			connection_state = ""
			negated = 0
			exact_allow = 1
			for (i = 1; i <= NF; i++) {
				if ($i == "-p" && i < NF) protocol = $(i + 1)
				if ($i == "--dport" && i < NF) destination_port = $(i + 1)
				if (($i == "-j" || $i == "-g") && i < NF) target = $(i + 1)
				if ($i == "--ctstate" && i < NF) connection_state = $(i + 1)
				if ($i == "!") negated = 1
			}
			if ($1 != "-A" || $2 != "INPUT") next
			for (i = 3; i <= NF; i++) {
				if (($i == "-p" || $i == "--dport" || $i == "-j") && i < NF) {
					i++
					continue
				}
				if ($i == "-m" && i < NF && $(i + 1) == "tcp") {
					i++
					continue
				}
				exact_allow = 0
			}
			if (target == "ACCEPT" && protocol == "tcp" && destination_port == wanted_port && !negated && exact_allow && !blocked) found = 1
			if (target == "DROP" || target == "REJECT") {
				if (protocol != "" && protocol != "tcp") next
				if (destination_port != "" && destination_port != wanted_port) next
				if (connection_state == "INVALID") next
				blocked = 1
			}
			if (target != "" && target != "ACCEPT" && target != "DROP" && target != "REJECT" && target != "LOG") blocked = 1
		}
		END { exit found ? 0 : 1 }
	' "${iptables_allow_file}"
}

iptables_chain_has_complex_terminal() {
	iptables_terminal_file=$1
	iptables_terminal_port=$2
	awk -v wanted_port="${iptables_terminal_port}" '
		{
			protocol = ""
			destination_port = ""
			target = ""
			connection_state = ""
			negated = 0
			for (i = 1; i <= NF; i++) {
				if ($i == "-p" && i < NF) protocol = $(i + 1)
				if ($i == "--dport" && i < NF) destination_port = $(i + 1)
				if (($i == "-j" || $i == "-g") && i < NF) target = $(i + 1)
				if ($i == "--ctstate" && i < NF) connection_state = $(i + 1)
				if ($i == "!") negated = 1
			}
			if ($1 != "-A" || $2 != "INPUT" || target == "" || target == "ACCEPT" || target == "LOG") next
			if (protocol != "" && protocol != "tcp" && protocol != "all" && !negated) next
			if (destination_port != "" && destination_port != wanted_port && !negated) next
			if ((target == "DROP" || target == "REJECT") && connection_state == "INVALID" && !negated) next
			complex = 1
		}
		END { exit complex ? 0 : 1 }
	' "${iptables_terminal_file}"
}

iptables_other_tables_have_complex_input() {
	iptables_tables_file=$1
	iptables_tables_port=$2
	awk -v wanted_port="${iptables_tables_port}" '
		/^\*/ { table_name = $1; next }
		$1 == "COMMIT" { table_name = ""; next }
		{
			rule_start = 0
			for (i = 1; i < NF; i++) if ($i == "-A" && $(i + 1) == "INPUT") rule_start = i
			if (rule_start == 0 || table_name == "*filter") next
			protocol = ""
			destination_port = ""
			target = ""
			negated = 0
			for (i = rule_start + 2; i <= NF; i++) {
				if ($i == "-p" && i < NF) protocol = $(i + 1)
				if ($i == "--dport" && i < NF) destination_port = $(i + 1)
				if (($i == "-j" || $i == "-g") && i < NF) target = $(i + 1)
				if ($i == "!") negated = 1
			}
			if (target == "") next
			if (protocol != "" && protocol != "tcp" && protocol != "all" && !negated) next
			if (destination_port != "" && destination_port != wanted_port && !negated) next
			if (target == "ACCEPT" || target == "LOG" || target == "NFLOG" || target == "MARK" || target == "CONNMARK" || target == "CT" || target == "NOTRACK" || target == "TRACE" || target == "SECMARK" || target == "CONNSECMARK" || target == "TCPMSS" || target == "TOS" || target == "TTL" || target == "DSCP" || target == "CLASSIFY") next
			complex = 1
		}
		END { exit complex ? 0 : 1 }
	' "${iptables_tables_file}"
}

load_iptables_service_config() {
	iptables_conf=/etc/conf.d/iptables
	if ! is_root_safe_file "${iptables_conf}"; then
		log_line WARN "${iptables_conf} is missing or unsafe; refusing to infer iptables persistence settings"
		return 1
	fi
	iptables_save_values="${WORK_DIR}/iptables-save-values"
	{
		sed -n 's/^[[:space:]]*IPTABLES_SAVE[[:space:]]*=[[:space:]]*"\([^"]*\)"[[:space:]]*$/\1/p' "${iptables_conf}"
		sed -n "s/^[[:space:]]*IPTABLES_SAVE[[:space:]]*=[[:space:]]*'\([^']*\)'[[:space:]]*$/\1/p" "${iptables_conf}"
		sed -n 's/^[[:space:]]*IPTABLES_SAVE[[:space:]]*=[[:space:]]*\(\/[A-Za-z0-9_.\/-]*\)[[:space:]]*$/\1/p' "${iptables_conf}"
	} >"${iptables_save_values}"
	iptables_save_count=$(sed '/^$/d' "${iptables_save_values}" | wc -l | tr -d '[:space:]')
	if [ "${iptables_save_count}" -ne 1 ]; then
		log_line WARN "${iptables_conf} must contain one simple absolute IPTABLES_SAVE assignment; found ${iptables_save_count}"
		return 1
	fi
	IPTABLES_RULES_FILE=$(sed '/^$/d' "${iptables_save_values}" | head -n 1)
	case "${IPTABLES_RULES_FILE}" in
		/*) ;;
		*) log_line WARN "IPTABLES_SAVE is not absolute: ${IPTABLES_RULES_FILE}"; return 1 ;;
	esac
	iptables_netns_line=$(sed -n 's/^[[:space:]]*netns[[:space:]]*=[[:space:]]*\(.*\)$/\1/p' "${iptables_conf}" | tail -n 1)
	case "${iptables_netns_line}" in
		""|'""'|"''") ;;
		*) log_line WARN "iptables is configured for a network namespace; automatic host firewall changes are unsafe"; return 1 ;;
	esac
	return 0
}

record_firewall_snapshot() {
	log_line INFO "capturing host firewall state for WebUI listener=${WEB_LISTEN}"
	if command -v firewall-cmd >/dev/null 2>&1; then
		firewall_snapshot="${WORK_DIR}/firewalld-state"
		{ firewall-cmd --state; firewall-cmd --get-active-zones; firewall-cmd --get-default-zone; } >"${firewall_snapshot}" 2>&1 || true
		append_output_to_log "firewalld state" "${firewall_snapshot}"
	fi
	if command -v ufw >/dev/null 2>&1; then
		firewall_snapshot="${WORK_DIR}/ufw-state"
		LC_ALL=C ufw status verbose >"${firewall_snapshot}" 2>&1 || true
		append_output_to_log "UFW state" "${firewall_snapshot}"
	fi
	if command -v awall >/dev/null 2>&1; then
		firewall_snapshot="${WORK_DIR}/awall-state"
		awall list --all >"${firewall_snapshot}" 2>&1 || true
		append_output_to_log "awall state" "${firewall_snapshot}"
	fi
	if command -v nft >/dev/null 2>&1; then
		firewall_snapshot="${WORK_DIR}/nftables-state"
		nft -a list ruleset >"${firewall_snapshot}" 2>&1 || true
		append_output_to_log "nftables ruleset" "${firewall_snapshot}"
	fi
	if command -v iptables >/dev/null 2>&1; then
		firewall_snapshot="${WORK_DIR}/iptables-state"
		iptables -S >"${firewall_snapshot}" 2>&1 || true
		append_output_to_log "iptables ruleset" "${firewall_snapshot}"
	fi
}

firewall_rule_warning() {
	firewall_warning_manager=$1
	log_line WARN "could not confirm an allow rule for ${WEB_PORT}/tcp in ${firewall_warning_manager}; remote WebUI access may require a host firewall rule"
	if [ "${FIREWALL_MODE}" = "check" ]; then
		log_line WARN "review the firewall snapshot in ${INSTALL_LOG}; use --open-firewall only when the selected firewall zone or ruleset serves a trusted management network"
	fi
}

clear_firewall_transaction() {
	FIREWALL_TRANSACTION=""
	FIREWALL_TRANSACTION_ACTIVE=0
	FIREWALL_ADDED_RUNTIME=0
	FIREWALL_ADDED_PERMANENT=0
	UFW_RULE_ADDED=0
	NFT_FRAGMENT_TOUCHED=0
	NFT_RUNTIME_HANDLE=""
	NFT_RUNTIME_ADDED=0
	NFT_OLD_RUNTIME_HANDLE=""
	NFT_OLD_RUNTIME_REMOVED=0
	IPTABLES_RULE_ADDED=0
	IPTABLES_RULES_TOUCHED=0
	IPTABLES_OLD_PORTS_FILE=""
	IPTABLES_REMOVED_PORTS_FILE=""
}

restore_nft_fragment() {
	[ -n "${NFT_PUBLISH_STAGE}" ] && rm -f -- "${NFT_PUBLISH_STAGE}" >>"${INSTALL_LOG}" 2>&1 || true
	[ "${NFT_FRAGMENT_TOUCHED}" -eq 1 ] || return 0
	if [ "${NFT_FRAGMENT_EXISTED}" -eq 1 ]; then
		rm -f -- "${NFT_FRAGMENT}" >>"${INSTALL_LOG}" 2>&1 || return 1
		cp -p "${NFT_FRAGMENT_BACKUP}" "${NFT_FRAGMENT}" >>"${INSTALL_LOG}" 2>&1 || return 1
	else
		rm -f -- "${NFT_FRAGMENT}" >>"${INSTALL_LOG}" 2>&1 || return 1
	fi
}

restore_iptables_rules_file() {
	[ -n "${IPTABLES_PUBLISH_STAGE}" ] && rm -f -- "${IPTABLES_PUBLISH_STAGE}" >>"${INSTALL_LOG}" 2>&1 || true
	[ "${IPTABLES_RULES_TOUCHED}" -eq 1 ] || return 0
	if [ "${IPTABLES_RULES_EXISTED}" -eq 1 ]; then
		rm -f -- "${IPTABLES_RULES_FILE}" >>"${INSTALL_LOG}" 2>&1 || return 1
		cp -p "${IPTABLES_RULES_BACKUP}" "${IPTABLES_RULES_FILE}" >>"${INSTALL_LOG}" 2>&1 || return 1
	else
		rm -f -- "${IPTABLES_RULES_FILE}" >>"${INSTALL_LOG}" 2>&1 || return 1
	fi
}

rollback_firewall_transaction() {
	[ "${FIREWALL_TRANSACTION_ACTIVE}" -eq 1 ] || return 0
	FIREWALL_TRANSACTION_ACTIVE=0
	log_line WARN "rolling back incomplete ${FIREWALL_TRANSACTION} firewall change"
	rollback_firewall_failed=0
	case "${FIREWALL_TRANSACTION}" in
		firewalld)
			if [ "${FIREWALL_ADDED_RUNTIME}" -eq 1 ] && firewall-cmd --zone="${FIREWALL_ZONE}" --query-port="${WEB_PORT}/tcp" >/dev/null 2>&1; then
				firewall-cmd --zone="${FIREWALL_ZONE}" --remove-port="${WEB_PORT}/tcp" >>"${INSTALL_LOG}" 2>&1 || rollback_firewall_failed=1
			fi
			if [ "${FIREWALL_ADDED_PERMANENT}" -eq 1 ] && firewall-cmd --permanent --zone="${FIREWALL_ZONE}" --query-port="${WEB_PORT}/tcp" >/dev/null 2>&1; then
				firewall-cmd --permanent --zone="${FIREWALL_ZONE}" --remove-port="${WEB_PORT}/tcp" >>"${INSTALL_LOG}" 2>&1 || rollback_firewall_failed=1
			fi
			;;
		ufw)
			ufw_rollback_state="${WORK_DIR}/ufw-rollback-state"
			LC_ALL=C ufw status >"${ufw_rollback_state}" 2>>"${INSTALL_LOG}" || true
			if [ "${UFW_RULE_ADDED}" -eq 1 ] && grep -Eq "(^|[[:space:]])${WEB_PORT}/tcp[[:space:]]+ALLOW([[:space:]]|$)" "${ufw_rollback_state}"; then
				ufw --force delete allow "${WEB_PORT}/tcp" >>"${INSTALL_LOG}" 2>&1 || rollback_firewall_failed=1
			fi
			;;
		nftables)
			if [ "${NFT_RUNTIME_ADDED}" -eq 1 ] && [ -z "${NFT_RUNTIME_HANDLE}" ] && command -v nft >/dev/null 2>&1; then
				nft_rollback_chain="${WORK_DIR}/nft-rollback-chain"
				nft -a list chain inet filter input >"${nft_rollback_chain}" 2>>"${INSTALL_LOG}" || true
				NFT_RUNTIME_HANDLE=$(sed -n '/tcp dport '"${WEB_PORT}"'.*accept.*comment "accept wwan-proxy WebUI"/s/.*# handle \([0-9][0-9]*\).*/\1/p' "${nft_rollback_chain}" | tail -n 1)
			fi
			if [ "${NFT_RUNTIME_ADDED}" -eq 1 ] && [ -n "${NFT_RUNTIME_HANDLE}" ]; then
				nft delete rule inet filter input handle "${NFT_RUNTIME_HANDLE}" >>"${INSTALL_LOG}" 2>&1 || rollback_firewall_failed=1
			fi
			restore_nft_fragment || rollback_firewall_failed=1
			if [ "${NFT_OLD_RUNTIME_REMOVED}" -eq 1 ] && [ "${NFT_FRAGMENT_EXISTED}" -eq 1 ]; then
				nft -f "${NFT_FRAGMENT_BACKUP}" >>"${INSTALL_LOG}" 2>&1 || rollback_firewall_failed=1
			fi
			;;
		iptables)
			if [ "${IPTABLES_RULE_ADDED}" -eq 1 ] && iptables -C INPUT -p tcp --dport "${WEB_PORT}" -m comment --comment "wwan-proxy WebUI" -j ACCEPT >/dev/null 2>&1; then
				iptables -D INPUT -p tcp --dport "${WEB_PORT}" -m comment --comment "wwan-proxy WebUI" -j ACCEPT >>"${INSTALL_LOG}" 2>&1 || rollback_firewall_failed=1
			fi
			restore_iptables_rules_file || rollback_firewall_failed=1
			if [ -n "${IPTABLES_REMOVED_PORTS_FILE}" ] && [ -f "${IPTABLES_REMOVED_PORTS_FILE}" ]; then
				while IFS= read -r iptables_removed_port; do
					case "${iptables_removed_port}" in *[!0-9]*|"") continue ;; esac
					iptables -I INPUT 1 -p tcp --dport "${iptables_removed_port}" -m comment --comment "wwan-proxy WebUI" -j ACCEPT >>"${INSTALL_LOG}" 2>&1 || rollback_firewall_failed=1
				done <"${IPTABLES_REMOVED_PORTS_FILE}"
			fi
			;;
	esac
	if [ "${rollback_firewall_failed}" -eq 1 ]; then
		log_line ERROR "firewall rollback was incomplete; inspect ${INSTALL_LOG} and the live ruleset immediately"
	else
		log_line WARN "incomplete firewall change was rolled back"
	fi
	clear_firewall_transaction
}

configure_firewalld() {
	firewalld_zones_file="${WORK_DIR}/firewalld-active-zones"
	firewalld_zone_names="${WORK_DIR}/firewalld-active-zone-names"
	firewall-cmd --get-active-zones >"${firewalld_zones_file}" 2>>"${INSTALL_LOG}" || return 1
	awk '/^[^[:space:]]/ { print $1 }' "${firewalld_zones_file}" >"${firewalld_zone_names}"
	firewalld_zone_count=$(wc -l <"${firewalld_zone_names}" | tr -d '[:space:]')
	if [ "${firewalld_zone_count}" -gt 1 ]; then
		log_line WARN "firewalld has multiple active zones; refusing to guess which zone should expose ${WEB_PORT}/tcp"
		return 1
	fi
	if [ "${firewalld_zone_count}" -eq 1 ]; then
		FIREWALL_ZONE=$(head -n 1 "${firewalld_zone_names}")
	else
		FIREWALL_ZONE=$(firewall-cmd --get-default-zone 2>>"${INSTALL_LOG}") || return 1
	fi
	firewalld_runtime_allowed=0
	firewalld_permanent_allowed=0
	firewall-cmd --zone="${FIREWALL_ZONE}" --query-port="${WEB_PORT}/tcp" >/dev/null 2>&1 && firewalld_runtime_allowed=1
	firewall-cmd --permanent --zone="${FIREWALL_ZONE}" --query-port="${WEB_PORT}/tcp" >/dev/null 2>&1 && firewalld_permanent_allowed=1
	if [ "${firewalld_runtime_allowed}" -eq 1 ] && [ "${firewalld_permanent_allowed}" -eq 1 ]; then
		log_line INFO "firewalld already allows ${WEB_PORT}/tcp at runtime and permanently in zone=${FIREWALL_ZONE}"
		return 0
	fi
	firewall_rule_warning "firewalld zone=${FIREWALL_ZONE}"
	[ "${FIREWALL_MODE}" = "open" ] || return 0
	FIREWALL_TRANSACTION="firewalld"
	FIREWALL_TRANSACTION_ACTIVE=1
	if [ "${firewalld_runtime_allowed}" -eq 0 ]; then
		FIREWALL_ADDED_RUNTIME=1
		run_step "activate firewalld WebUI rule in zone ${FIREWALL_ZONE}" \
			firewall-cmd --zone="${FIREWALL_ZONE}" --add-port="${WEB_PORT}/tcp" || { rollback_firewall_transaction; return 1; }
	fi
	if [ "${firewalld_permanent_allowed}" -eq 0 ]; then
		FIREWALL_ADDED_PERMANENT=1
		run_step "persist firewalld WebUI rule in zone ${FIREWALL_ZONE}" \
			firewall-cmd --permanent --zone="${FIREWALL_ZONE}" --add-port="${WEB_PORT}/tcp" || { rollback_firewall_transaction; return 1; }
	fi
	if ! firewall-cmd --zone="${FIREWALL_ZONE}" --query-port="${WEB_PORT}/tcp" >/dev/null 2>&1 || \
		! firewall-cmd --permanent --zone="${FIREWALL_ZONE}" --query-port="${WEB_PORT}/tcp" >/dev/null 2>&1; then
		rollback_firewall_transaction
		return 1
	fi
	clear_firewall_transaction
	CURRENT_STEP="firewall"
	log_line WARN "opened ${WEB_PORT}/tcp through firewalld zone=${FIREWALL_ZONE}; restrict the zone to trusted administrators"
	return 0
}

configure_ufw() {
	ufw_state_file="${WORK_DIR}/ufw-active-state"
	LC_ALL=C ufw status >"${ufw_state_file}" 2>>"${INSTALL_LOG}" || return 1
	if grep -Eq "(^|[[:space:]])${WEB_PORT}/tcp[[:space:]]+ALLOW([[:space:]]|$)" "${ufw_state_file}"; then
		log_line INFO "UFW already contains an allow rule for ${WEB_PORT}/tcp"
		return 0
	fi
	firewall_rule_warning "UFW"
	[ "${FIREWALL_MODE}" = "open" ] || return 0
	FIREWALL_TRANSACTION="ufw"
	FIREWALL_TRANSACTION_ACTIVE=1
	UFW_RULE_ADDED=1
	run_step "allow WebUI ${WEB_PORT}/tcp through UFW" \
		ufw allow "${WEB_PORT}/tcp" comment "wwan-proxy WebUI" || { rollback_firewall_transaction; return 1; }
	if ! LC_ALL=C ufw status >"${ufw_state_file}" 2>>"${INSTALL_LOG}" || \
		! grep -Eq "(^|[[:space:]])${WEB_PORT}/tcp[[:space:]]+ALLOW([[:space:]]|$)" "${ufw_state_file}"; then
		rollback_firewall_transaction
		return 1
	fi
	clear_firewall_transaction
	CURRENT_STEP="firewall"
	log_line WARN "opened ${WEB_PORT}/tcp through UFW; restrict the rule to a trusted source network"
	return 0
}

write_nft_fragment() {
	nft_fragment_path=$1
	nft_fragment_port=$2
	{
		printf '%s\n' '# Managed by the wwan-proxy Alpine installer.'
		printf '%s\n' 'table inet filter {'
		printf '\tchain input {\n'
		printf '\t\ttcp dport %s accept comment "accept wwan-proxy WebUI"\n' "${nft_fragment_port}"
		printf '\t}\n'
		printf '%s\n' '}'
	} >"${nft_fragment_path}"
}

nft_chain_has_complex_terminal() {
	nft_terminal_chain=$1
	sed -E 's/comment "[^"]*"//g; s/# handle [0-9]+//g; s/ct state invalid[[:space:]]+drop[[:space:]\\;]*//g; s/tcp dport 113[[:space:]]+reject[^;}]*;?//g' "${nft_terminal_chain}" | \
		grep -E '(^|[[:space:];])(drop|reject|return|jump|goto|queue)([[:space:];]|$)' | \
		grep -Ev 'type filter hook.*policy (accept|drop);' \
		>/dev/null
}

nft_fragment_has_input_terminal() {
	nft_input_fragment=$1
	nft_allow_root_flush=${2:-0}
	awk -v allow_root_flush="${nft_allow_root_flush}" '
		function strip_hash_outside_quotes(value, output, quoted, escaped, position, character) {
			output = ""
			quoted = 0
			escaped = 0
			for (position = 1; position <= length(value); position++) {
				character = substr(value, position, 1)
				if (!quoted && character == "#") break
				output = output character
				if (!quoted) {
					if (character == "\"") quoted = 1
				} else if (escaped) {
					escaped = 0
				} else if (character == "\\") {
					escaped = 1
				} else if (character == "\"") {
					quoted = 0
				}
			}
			return output
		}
		function strip_nft_comment_expressions(value, output, position, after_keyword, character, escaped, previous) {
			output = ""
			position = 1
			while (position <= length(value)) {
				previous = position == 1 ? "" : substr(value, position - 1, 1)
				if (substr(value, position, 7) == "comment" && previous !~ /[[:alnum:]_]/) {
					after_keyword = position + 7
					if (substr(value, after_keyword, 1) ~ /[[:space:]]/) {
						while (substr(value, after_keyword, 1) ~ /[[:space:]]/) after_keyword++
						if (substr(value, after_keyword, 1) == "\"") {
							after_keyword++
							escaped = 0
							while (after_keyword <= length(value)) {
								character = substr(value, after_keyword, 1)
								after_keyword++
								if (escaped) {
									escaped = 0
								} else if (character == "\\") {
									escaped = 1
								} else if (character == "\"") {
									break
								}
							}
							position = after_keyword
							continue
						}
					}
				}
				output = output substr(value, position, 1)
				position++
			}
			return output
		}
		{
			line = strip_hash_outside_quotes($0)
			line = strip_nft_comment_expressions(line)
			if (line ~ /(^|[[:space:];])(flush|delete)[[:space:]]+(ruleset|table[[:space:]]+inet[[:space:]]+filter|chain[[:space:]]+inet[[:space:]]+filter[[:space:]]+input)([[:space:];]|$)/) {
				root_flush = line ~ /^[[:space:]]*flush[[:space:]]+ruleset[[:space:];]*$/
				if (!allow_root_flush || !root_flush) unsafe = 1
			}
			if (line ~ /(^|[[:space:];])(add|insert|replace)[[:space:]]+rule[[:space:]]+inet[[:space:]]+filter[[:space:]]+input([[:space:]]|$)/ && line ~ /(^|[[:space:];])(drop|reject|return|jump|goto|queue)([[:space:];]|$)/) unsafe = 1
			brace_copy = line
			opens = gsub(/\{/, "", brace_copy)
			brace_copy = line
			closes = gsub(/\}/, "", brace_copy)
			if (!in_table && line ~ /table[[:space:]]+inet[[:space:]]+filter[[:space:]]*\{/) {
				in_table = 1
				table_depth = depth + 1
			}
			if (in_table && !in_chain && line ~ /chain[[:space:]]+input[[:space:]]*\{/) {
				in_chain = 1
				chain_depth = depth + opens
			}
			check_line = line
			gsub(/policy[[:space:]]+(accept|drop)[[:space:]]*;/, "", check_line)
			gsub(/ct[[:space:]]+state[[:space:]]+invalid[[:space:]]+drop[[:space:]\\;]*/, "", check_line)
			gsub(/tcp[[:space:]]+dport[[:space:]]+113[[:space:]]+reject[^;}]*;?/, "", check_line)
			if (in_chain && check_line ~ /(^|[[:space:];])(drop|reject|return|jump|goto|queue)([[:space:];]|$)/) unsafe = 1
			depth += opens - closes
			if (in_chain && depth < chain_depth) in_chain = 0
			if (in_table && depth < table_depth) in_table = 0
		}
		END { exit unsafe ? 0 : 1 }
	' "${nft_input_fragment}"
}

nft_persistent_fragments_are_safe() {
	for nft_policy_fragment in /etc/nftables.d/*.nft /var/lib/nftables/*.nft; do
		if [ ! -e "${nft_policy_fragment}" ] && [ ! -L "${nft_policy_fragment}" ]; then
			continue
		fi
		if ! is_root_safe_file "${nft_policy_fragment}"; then
			log_line WARN "nftables fragment is unsafe or writable by non-root users: ${nft_policy_fragment}"
			return 1
		fi
		if [ "${nft_policy_fragment}" != "${NFT_FRAGMENT}" ]; then
			if nft_fragment_has_input_terminal "${nft_policy_fragment}"; then
				log_line WARN "nftables fragment has terminal control flow that this installer cannot safely order before: ${nft_policy_fragment}"
			else
				log_line WARN "unmanaged nftables startup fragment prevents the installer from proving persistent rule order: ${nft_policy_fragment}"
			fi
			return 1
		fi
	done
	return 0
}

configure_standard_nftables() {
	nft_chain_file="${WORK_DIR}/nft-input-chain"
	nft_full_ruleset_file="${WORK_DIR}/nft-full-ruleset"
	if ! nft -a list ruleset >"${nft_full_ruleset_file}" 2>>"${INSTALL_LOG}"; then
		return 2
	fi
	nft_sanitized_ruleset="${WORK_DIR}/nft-full-ruleset-sanitized"
	sed -E 's/comment "[^"]*"//g; s/# handle [0-9]+//g' "${nft_full_ruleset_file}" >"${nft_sanitized_ruleset}"
	nft_input_hook_count=$(grep -Ec 'type[[:space:]]+filter[[:space:]]+hook[[:space:]]+input([[:space:]]|;)' "${nft_sanitized_ruleset}" || true)
	if [ "${nft_input_hook_count}" -ne 1 ]; then
		log_line WARN "nftables has ${nft_input_hook_count} filter input base chains; one allow verdict cannot prove reachability across multiple hooks"
		[ "${FIREWALL_MODE}" = "open" ] && return 1
		return 0
	fi
	if ! nft -a list chain inet filter input >"${nft_chain_file}" 2>>"${INSTALL_LOG}"; then
		return 2
	fi
	nft_sanitized_chain="${WORK_DIR}/nft-input-chain-sanitized"
	sed -E 's/comment "[^"]*"//g; s/# handle [0-9]+//g' "${nft_chain_file}" >"${nft_sanitized_chain}"
	if nft_chain_has_complex_terminal "${nft_chain_file}"; then
		log_line WARN "nftables inet/filter/input contains custom terminal control flow; textual port rules cannot prove remote reachability"
		[ "${FIREWALL_MODE}" = "open" ] && return 1
		return 0
	fi
	nft_policy_count=$(grep -Ec 'type[[:space:]]+filter[[:space:]]+hook[[:space:]]+input.*;[[:space:]]*policy[[:space:]]+(accept|drop);' "${nft_sanitized_chain}" || true)
	if [ "${nft_policy_count}" -ne 1 ]; then
		log_line WARN "nftables inet/filter/input has ${nft_policy_count} recognizable base-chain policy declarations"
		[ "${FIREWALL_MODE}" = "open" ] && return 1
		return 0
	fi
	nft_runtime_allowed=0
	if grep -Eq 'type[[:space:]]+filter[[:space:]]+hook[[:space:]]+input.*;[[:space:]]*policy[[:space:]]+accept;' "${nft_sanitized_chain}"; then
		nft_runtime_allowed=1
		log_line INFO "nftables inet/filter/input has an ACCEPT policy and no applicable terminal deny rule"
	elif ! grep -Eq 'type[[:space:]]+filter[[:space:]]+hook[[:space:]]+input.*;[[:space:]]*policy[[:space:]]+drop;' "${nft_sanitized_chain}"; then
		log_line WARN "nftables inet/filter/input policy is neither a recognizable ACCEPT nor DROP policy"
		[ "${FIREWALL_MODE}" = "open" ] && return 1
		return 0
	fi
	grep -Eq "tcp dport ${WEB_PORT}([[:space:]]|$).*accept([[:space:]]|$)" "${nft_sanitized_chain}" && nft_runtime_allowed=1
	if [ "${nft_runtime_allowed}" -eq 1 ]; then
		log_line INFO "nftables inet/filter/input currently allows ${WEB_PORT}/tcp"
	else
		firewall_rule_warning "nftables inet/filter/input"
	fi
	[ "${FIREWALL_MODE}" = "open" ] || return 0

	NFT_FRAGMENT="/etc/nftables.d/50_wwan-proxy.nft"
	NFT_FRAGMENT_STAGE="${WORK_DIR}/50_wwan-proxy.nft"
	NFT_FRAGMENT_BACKUP="${WORK_DIR}/50_wwan-proxy.nft.previous"
	NFT_FRAGMENT_EXISTED=0
	nft_managed_fragment=0
	nft_previous_port=""
	if [ -e "${NFT_FRAGMENT}" ] || [ -L "${NFT_FRAGMENT}" ]; then
		if ! is_root_safe_file "${NFT_FRAGMENT}"; then
			log_line WARN "refusing unsafe nftables fragment: ${NFT_FRAGMENT}"
			return 1
		fi
		nft_previous_port=$(sed -n 's/^[[:space:]]*tcp dport \([0-9][0-9]*\) accept comment "accept wwan-proxy WebUI"$/\1/p' "${NFT_FRAGMENT}")
		case "${nft_previous_port}" in
			*[!0-9]*|"") ;;
			*)
				nft_expected_previous="${WORK_DIR}/50_wwan-proxy.expected-previous"
				write_nft_fragment "${nft_expected_previous}" "${nft_previous_port}" || return 1
				cmp -s "${nft_expected_previous}" "${NFT_FRAGMENT}" && nft_managed_fragment=1
				;;
		esac
		if [ "${nft_managed_fragment}" -ne 1 ]; then
			log_line WARN "${NFT_FRAGMENT} is not an installer-managed fragment; refusing to overwrite it"
			return 1
		fi
		NFT_FRAGMENT_EXISTED=1
		cp -p "${NFT_FRAGMENT}" "${NFT_FRAGMENT_BACKUP}" >>"${INSTALL_LOG}" 2>&1 || return 1
	fi
	if ! rc-service nftables status >/dev/null 2>&1; then
		log_line WARN "nftables rules exist but the OpenRC service is not started; refusing to modify an unowned runtime ruleset"
		return 1
	fi
	if ! is_root_safe_file /etc/nftables.nft || \
		! grep -Eq '^[[:space:]]*include[[:space:]]+"/etc/nftables\.d/\*\.nft"[[:space:]]*(#.*)?$' /etc/nftables.nft; then
		log_line WARN "nftables does not use the root-controlled Alpine /etc/nftables.d include layout; automatic modification skipped"
		return 1
	fi
	nft_unexpected_includes="${WORK_DIR}/nft-unexpected-includes"
	awk '
		{
			line = $0
			sub(/#.*/, "", line)
			gsub(/^[[:space:]]+|[[:space:]]+$/, "", line)
			if (line !~ /^include[[:space:]]+/) next
			if (line != "include \"/etc/nftables.d/*.nft\"" && line != "include \"/var/lib/nftables/*.nft\"") print line
		}
	' /etc/nftables.nft >"${nft_unexpected_includes}"
	if [ -s "${nft_unexpected_includes}" ]; then
		append_output_to_log "unexpected nftables startup includes" "${nft_unexpected_includes}"
		log_line WARN "nftables startup config includes paths outside the standard Alpine-owned layout; automatic modification skipped"
		return 1
	fi
	if nft_fragment_has_input_terminal /etc/nftables.nft 1; then
		log_line WARN "/etc/nftables.nft contains persistent input control flow that may precede the installer fragment; automatic modification skipped"
		return 1
	fi
	nft_manifest="${WORK_DIR}/nftables-apk-manifest"
	if ! LC_ALL=C apk manifest nftables >"${nft_manifest}" 2>>"${INSTALL_LOG}"; then
		log_line WARN "could not read the installed nftables package manifest; automatic modification skipped"
		return 1
	fi
	nft_manifest_digest=$(awk '$2 == "etc/nftables.nft" { print $1 }' "${nft_manifest}")
	nft_manifest_count=$(printf '%s\n' "${nft_manifest_digest}" | sed '/^$/d' | wc -l | tr -d '[:space:]')
	if [ "${nft_manifest_count}" -ne 1 ]; then
		log_line WARN "the installed nftables package manifest has ${nft_manifest_count} entries for /etc/nftables.nft; automatic modification skipped"
		return 1
	fi
	case "${nft_manifest_digest}" in
		sha1:*) nft_expected_digest=${nft_manifest_digest#sha1:}; nft_actual_digest=$(sha1sum /etc/nftables.nft | awk '{ print $1 }') ;;
		sha256:*) nft_expected_digest=${nft_manifest_digest#sha256:}; nft_actual_digest=$(sha256sum /etc/nftables.nft | awk '{ print $1 }') ;;
		sha512:*) nft_expected_digest=${nft_manifest_digest#sha512:}; nft_actual_digest=$(sha512sum /etc/nftables.nft | awk '{ print $1 }') ;;
		*) log_line WARN "unsupported nftables manifest digest: ${nft_manifest_digest}"; return 1 ;;
	esac
	if [ "${nft_actual_digest}" != "${nft_expected_digest}" ]; then
		log_line WARN "/etc/nftables.nft differs from the Alpine package-managed baseline; automatic modification skipped"
		return 1
	fi
	if ! is_root_safe_directory /etc/nftables.d; then
		log_line WARN "/etc/nftables.d is missing or writable by non-root users; automatic modification skipped"
		return 1
	fi
	if ! is_root_safe_directory /var/lib/nftables; then
		log_line WARN "/var/lib/nftables is missing or writable by non-root users; automatic modification skipped"
		return 1
	fi
	if ! nft_persistent_fragments_are_safe; then
		return 1
	fi

	NFT_OLD_RUNTIME_HANDLE=""
	if [ -n "${nft_previous_port}" ] && [ "${nft_previous_port}" != "${WEB_PORT}" ]; then
		nft_old_handles=$(sed -n '/tcp dport '"${nft_previous_port}"'.*accept.*comment "accept wwan-proxy WebUI"/s/.*# handle \([0-9][0-9]*\).*/\1/p' "${nft_chain_file}")
		nft_old_handle_count=$(printf '%s\n' "${nft_old_handles}" | sed '/^$/d' | wc -l | tr -d '[:space:]')
		if [ "${nft_old_handle_count}" -gt 1 ]; then
			log_line WARN "multiple installer-managed nftables rules exist for old port ${nft_previous_port}; refusing to guess which handle to remove"
			return 1
		fi
		[ "${nft_old_handle_count}" -eq 1 ] && NFT_OLD_RUNTIME_HANDLE=${nft_old_handles}
	fi

	write_nft_fragment "${NFT_FRAGMENT_STAGE}" "${WEB_PORT}" || return 1
	if [ "${NFT_FRAGMENT_EXISTED}" -eq 1 ] && [ "${nft_runtime_allowed}" -eq 1 ] && \
		cmp -s "${NFT_FRAGMENT_STAGE}" "${NFT_FRAGMENT}"; then
		if ! run_step "validate existing nftables startup rule for wwan-proxy WebUI" nft -c -f /etc/nftables.nft; then
			return 1
		fi
		log_line INFO "nftables already allows ${WEB_PORT}/tcp in both the live and persistent installer-managed rulesets"
		return 0
	fi
	NFT_PUBLISH_STAGE="/etc/nftables.d/.50_wwan-proxy.nft.install-${RUN_ID}"
	if [ -e "${NFT_PUBLISH_STAGE}" ] || [ -L "${NFT_PUBLISH_STAGE}" ]; then
		log_line WARN "refusing unexpected nftables staging path: ${NFT_PUBLISH_STAGE}"
		return 1
	fi
	FIREWALL_TRANSACTION="nftables"
	FIREWALL_TRANSACTION_ACTIVE=1
	if ! cp "${NFT_FRAGMENT_STAGE}" "${NFT_PUBLISH_STAGE}" >>"${INSTALL_LOG}" 2>&1 || \
		! chown root:root "${NFT_PUBLISH_STAGE}" >>"${INSTALL_LOG}" 2>&1 || \
		! chmod 0644 "${NFT_PUBLISH_STAGE}" >>"${INSTALL_LOG}" 2>&1; then
		rollback_firewall_transaction
		return 1
	fi
	NFT_FRAGMENT_TOUCHED=1
	if ! mv -f "${NFT_PUBLISH_STAGE}" "${NFT_FRAGMENT}" >>"${INSTALL_LOG}" 2>&1; then
		rollback_firewall_transaction
		return 1
	fi
	if ! run_step "validate nftables startup rules with wwan-proxy WebUI access" nft -c -f /etc/nftables.nft; then
		rollback_firewall_transaction
		return 1
	fi

	if [ "${nft_runtime_allowed}" -eq 0 ]; then
		nft_apply_output="${WORK_DIR}/nft-runtime-apply"
		NFT_RUNTIME_ADDED=1
		log_line INFO "STEP: add the WebUI rule to the live nftables chain without flushing runtime-only rules"
		if ! nft -ae -f "${NFT_FRAGMENT_STAGE}" >"${nft_apply_output}" 2>&1; then
			append_output_to_log "nftables incremental apply" "${nft_apply_output}"
			rollback_firewall_transaction
			return 1
		fi
		append_output_to_log "nftables incremental apply" "${nft_apply_output}"
		NFT_RUNTIME_HANDLE=$(sed -n '/^add rule inet filter input tcp dport '"${WEB_PORT}"' accept comment "accept wwan-proxy WebUI" # handle /s/.*# handle \([0-9][0-9]*\).*/\1/p' "${nft_apply_output}" | tail -n 1)
	fi
	if ! nft -a list chain inet filter input >"${nft_chain_file}" 2>>"${INSTALL_LOG}"; then
		rollback_firewall_transaction
		return 1
	fi
	sed -E 's/comment "[^"]*"//g; s/# handle [0-9]+//g' "${nft_chain_file}" >"${nft_sanitized_chain}"
	if ! grep -Eq "tcp dport ${WEB_PORT}([[:space:]]|$).*accept([[:space:]]|$)" "${nft_sanitized_chain}"; then
		rollback_firewall_transaction
		return 1
	fi
	if [ "${NFT_RUNTIME_ADDED}" -eq 1 ] && \
		! grep -Eq "tcp dport ${WEB_PORT}([[:space:]]|$).*accept.*comment \"accept wwan-proxy WebUI\"" "${nft_chain_file}"; then
		rollback_firewall_transaction
		return 1
	fi
	if [ "${NFT_RUNTIME_ADDED}" -eq 1 ] && [ -z "${NFT_RUNTIME_HANDLE}" ]; then
		NFT_RUNTIME_HANDLE=$(sed -n '/tcp dport '"${WEB_PORT}"'.*accept.*comment "accept wwan-proxy WebUI"/s/.*# handle \([0-9][0-9]*\).*/\1/p' "${nft_chain_file}" | tail -n 1)
		if [ -z "${NFT_RUNTIME_HANDLE}" ]; then
			rollback_firewall_transaction
			return 1
		fi
	fi
	if [ -n "${NFT_OLD_RUNTIME_HANDLE}" ]; then
		if ! nft delete rule inet filter input handle "${NFT_OLD_RUNTIME_HANDLE}" >>"${INSTALL_LOG}" 2>&1; then
			rollback_firewall_transaction
			return 1
		fi
		NFT_OLD_RUNTIME_REMOVED=1
	fi
	clear_firewall_transaction
	CURRENT_STEP="firewall"
	log_line WARN "opened ${WEB_PORT}/tcp in Alpine nftables; restrict access to a trusted source or interface"
	return 0
}

configure_standard_iptables() {
	iptables_chain_raw="${WORK_DIR}/iptables-input-chain-raw"
	if ! iptables -S INPUT >"${iptables_chain_raw}" 2>>"${INSTALL_LOG}"; then
		return 2
	fi
	iptables_chain_sanitized="${WORK_DIR}/iptables-input-chain-sanitized"
	iptables_chain_file="${WORK_DIR}/iptables-input-chain"
	sanitize_iptables_rules "${iptables_chain_raw}" "${iptables_chain_sanitized}" || return 2
	extract_iptables_filter_input_rules "${iptables_chain_sanitized}" "${iptables_chain_file}" || return 2
	iptables_all_tables_raw="${WORK_DIR}/iptables-all-tables-raw"
	iptables_all_tables_file="${WORK_DIR}/iptables-all-tables"
	if ! iptables-save >"${iptables_all_tables_raw}" 2>>"${INSTALL_LOG}"; then
		return 2
	fi
	sanitize_iptables_rules "${iptables_all_tables_raw}" "${iptables_all_tables_file}" || return 2
	if iptables_other_tables_have_complex_input "${iptables_all_tables_file}" "${WEB_PORT}"; then
		log_line WARN "iptables has a non-filter INPUT terminal or custom chain that may block ${WEB_PORT}/tcp; automatic reachability cannot be proven"
		[ "${FIREWALL_MODE}" = "open" ] && return 1
		return 0
	fi
	iptables_runtime_allowed=0
	iptables_runtime_needs_rule=0
	if iptables_chain_allows_tcp_port "${iptables_chain_file}" "${WEB_PORT}"; then
		iptables_runtime_allowed=1
		log_line INFO "iptables already contains an unrestricted explicit allow rule for ${WEB_PORT}/tcp"
	elif iptables_chain_has_complex_terminal "${iptables_chain_file}" "${WEB_PORT}"; then
		log_line WARN "iptables INPUT contains a custom deny or control-flow rule that may apply to ${WEB_PORT}/tcp; refusing to insert a broader allow rule ahead of it"
		[ "${FIREWALL_MODE}" = "open" ] && return 1
		return 0
	elif grep -Eq '^-P INPUT (DROP|REJECT)$' "${iptables_chain_file}"; then
		iptables_runtime_needs_rule=1
		firewall_rule_warning "iptables INPUT"
	else
		log_line INFO "iptables has no explicit INPUT deny policy requiring a dedicated ${WEB_PORT}/tcp rule"
	fi
	[ "${FIREWALL_MODE}" = "open" ] || return 0
	if ! rc-service iptables status >/dev/null 2>&1; then
		if [ "${iptables_runtime_allowed}" -eq 1 ] || [ "${iptables_runtime_needs_rule}" -eq 0 ]; then
			log_line INFO "iptables OpenRC is inactive, but the live INPUT ruleset does not block ${WEB_PORT}/tcp"
			return 0
		fi
		log_line WARN "iptables OpenRC service is not started; refusing to modify an unowned runtime ruleset"
		return 1
	fi
	load_iptables_service_config || return 1
	iptables_rules_dir=$(dirname "${IPTABLES_RULES_FILE}")
	iptables_rules_name=$(basename "${IPTABLES_RULES_FILE}")
	if ! is_root_safe_directory "${iptables_rules_dir}"; then
		log_line WARN "${iptables_rules_dir} is missing or writable by non-root users; refusing to persist an iptables rule"
		return 1
	fi
	if [ ! -e "${IPTABLES_RULES_FILE}" ] && [ ! -L "${IPTABLES_RULES_FILE}" ]; then
		log_line WARN "${IPTABLES_RULES_FILE} is missing; refusing to derive persistent rules from possibly temporary runtime state"
		return 1
	fi
	if ! is_root_safe_file "${IPTABLES_RULES_FILE}"; then
		log_line WARN "refusing unsafe iptables rules file: ${IPTABLES_RULES_FILE}"
		return 1
	fi
	if head -n 1 "${IPTABLES_RULES_FILE}" | grep -Fqx '# rules-save generated by awall'; then
		log_line WARN "${IPTABLES_RULES_FILE} is managed by awall; modify and activate the awall policy instead"
		return 1
	fi
	if ! grep -Fqx '*filter' "${IPTABLES_RULES_FILE}" || ! grep -Eq '^:INPUT (ACCEPT|DROP|REJECT) ' "${IPTABLES_RULES_FILE}"; then
		log_line WARN "${IPTABLES_RULES_FILE} has no standard filter/INPUT section; automatic modification skipped"
		return 1
	fi
	iptables_persistent_sanitized="${WORK_DIR}/iptables-rules-save-sanitized"
	iptables_persistent_input="${WORK_DIR}/iptables-rules-save-input"
	sanitize_iptables_rules "${IPTABLES_RULES_FILE}" "${iptables_persistent_sanitized}" || return 1
	extract_iptables_filter_input_rules "${iptables_persistent_sanitized}" "${iptables_persistent_input}" || return 1
	if iptables_other_tables_have_complex_input "${iptables_persistent_sanitized}" "${WEB_PORT}"; then
		log_line WARN "${IPTABLES_RULES_FILE} contains a non-filter INPUT terminal or custom chain; automatic modification skipped"
		return 1
	fi
	iptables_persistent_allowed=0
	if iptables_chain_allows_tcp_port "${iptables_persistent_input}" "${WEB_PORT}"; then
		iptables_persistent_allowed=1
		log_line INFO "${IPTABLES_RULES_FILE} already persists an unrestricted ${WEB_PORT}/tcp allow before applicable terminal rules"
	elif iptables_chain_has_complex_terminal "${iptables_persistent_input}" "${WEB_PORT}"; then
		log_line WARN "${IPTABLES_RULES_FILE} contains a persistent INPUT custom deny or control-flow rule that may precede the WebUI allow; automatic modification skipped"
		return 1
	elif grep -Eq '^:INPUT ACCEPT ' "${iptables_persistent_sanitized}"; then
		iptables_persistent_allowed=1
		log_line INFO "${IPTABLES_RULES_FILE} persists an ACCEPT policy with no applicable terminal deny rule"
	else
		firewall_rule_warning "persistent iptables INPUT"
	fi
	if { [ "${iptables_runtime_allowed}" -eq 1 ] || [ "${iptables_runtime_needs_rule}" -eq 0 ]; } && \
		[ "${iptables_persistent_allowed}" -eq 1 ]; then
		log_line INFO "iptables allows ${WEB_PORT}/tcp in both the live and startup rulesets"
		return 0
	fi
	IPTABLES_RULES_EXISTED=1
	IPTABLES_RULES_BACKUP="${WORK_DIR}/iptables-rules-save.previous"
	cp -p "${IPTABLES_RULES_FILE}" "${IPTABLES_RULES_BACKUP}" >>"${INSTALL_LOG}" 2>&1 || return 1
	iptables_rules_stage="${WORK_DIR}/iptables-rules-save"
	awk -v web_port="${WEB_PORT}" '
		BEGIN { in_filter = 0; inserted = 0 }
		$0 == "*filter" { in_filter = 1 }
		in_filter && /--comment "wwan-proxy WebUI"/ { next }
		in_filter && !inserted && ($1 == "-A" || ($1 ~ /^\[[0-9]+:[0-9]+\]$/ && $2 == "-A") || $0 == "COMMIT") {
			print "-A INPUT -p tcp -m tcp --dport " web_port " -m comment --comment \"wwan-proxy WebUI\" -j ACCEPT"
			inserted = 1
		}
		{ print }
		in_filter && $0 == "COMMIT" { in_filter = 0 }
		END { if (!inserted) exit 1 }
	' "${IPTABLES_RULES_FILE}" >"${iptables_rules_stage}" || return 1
	iptables_restore_test="${WORK_DIR}/iptables-restore-test"
	if ! iptables-restore --test <"${iptables_rules_stage}" >"${iptables_restore_test}" 2>&1; then
		append_output_to_log "validate persistent iptables rules" "${iptables_restore_test}"
		return 1
	fi
	IPTABLES_OLD_PORTS_FILE="${WORK_DIR}/iptables-old-managed-ports"
	IPTABLES_REMOVED_PORTS_FILE="${WORK_DIR}/iptables-removed-managed-ports"
	sed -n '/--comment "wwan-proxy WebUI"/s/.*--dport \([0-9][0-9]*\).*/\1/p' "${iptables_chain_raw}" >"${IPTABLES_OLD_PORTS_FILE}"
	: >"${IPTABLES_REMOVED_PORTS_FILE}"
	IPTABLES_PUBLISH_STAGE="${iptables_rules_dir}/.${iptables_rules_name}.install-${RUN_ID}"
	if [ -e "${IPTABLES_PUBLISH_STAGE}" ] || [ -L "${IPTABLES_PUBLISH_STAGE}" ]; then
		log_line WARN "refusing unexpected iptables staging path: ${IPTABLES_PUBLISH_STAGE}"
		return 1
	fi
	FIREWALL_TRANSACTION="iptables"
	FIREWALL_TRANSACTION_ACTIVE=1
	if ! cp "${iptables_rules_stage}" "${IPTABLES_PUBLISH_STAGE}" >>"${INSTALL_LOG}" 2>&1 || \
		! chown root:root "${IPTABLES_PUBLISH_STAGE}" >>"${INSTALL_LOG}" 2>&1 || \
		! chmod 0600 "${IPTABLES_PUBLISH_STAGE}" >>"${INSTALL_LOG}" 2>&1; then
		rollback_firewall_transaction
		return 1
	fi
	IPTABLES_RULES_TOUCHED=1
	if ! mv -f "${IPTABLES_PUBLISH_STAGE}" "${IPTABLES_RULES_FILE}" >>"${INSTALL_LOG}" 2>&1; then
		rollback_firewall_transaction
		return 1
	fi
	while IFS= read -r iptables_old_port; do
		case "${iptables_old_port}" in *[!0-9]*|"") continue ;; esac
		if ! iptables -D INPUT -p tcp --dport "${iptables_old_port}" -m comment --comment "wwan-proxy WebUI" -j ACCEPT >>"${INSTALL_LOG}" 2>&1; then
			rollback_firewall_transaction
			return 1
		fi
		printf '%s\n' "${iptables_old_port}" >>"${IPTABLES_REMOVED_PORTS_FILE}"
	done <"${IPTABLES_OLD_PORTS_FILE}"
	IPTABLES_RULE_ADDED=1
	run_step "allow WebUI ${WEB_PORT}/tcp through iptables" \
		iptables -I INPUT 1 -p tcp --dport "${WEB_PORT}" -m comment --comment "wwan-proxy WebUI" -j ACCEPT || { rollback_firewall_transaction; return 1; }
	if ! iptables -C INPUT -p tcp --dport "${WEB_PORT}" -m comment --comment "wwan-proxy WebUI" -j ACCEPT >/dev/null 2>&1 || \
		! grep -Fq -- '--comment "wwan-proxy WebUI"' "${IPTABLES_RULES_FILE}"; then
		rollback_firewall_transaction
		return 1
	fi
	clear_firewall_transaction
	CURRENT_STEP="firewall"
	log_line WARN "opened ${WEB_PORT}/tcp through iptables; restrict the rule to a trusted source network"
	return 0
}

configure_host_firewall() {
	CURRENT_STEP="firewall"
	if ! read_effective_web_listener; then
		log_line WARN "could not determine the effective WebUI listener; host firewall inspection skipped"
		[ "${FIREWALL_MODE}" = "open" ] && return 1
		return 0
	fi
	log_line INFO "effective Alpine WebUI listener=${WEB_LISTEN}"
	if [ "${FIREWALL_MODE}" = "skip" ]; then
		log_line WARN "host firewall inspection skipped by request"
		return 0
	fi
	if is_loopback_web_host; then
		log_line INFO "WebUI is loopback-only; no inbound host firewall rule is needed"
		return 0
	fi
	log_line INFO "remote access URL=http://<Alpine-host-IP>:${WEB_PORT}"
	log_line WARN "if no administrator has been initialized, the first WebUI visitor can create it; expose ${WEB_PORT}/tcp only to a trusted management network"
	record_firewall_snapshot

	firewalld_active=0
	ufw_active=0
	awall_active=0
	awall_configured=0
	awall_owned_rules=0
	iptables_service_active=0
	nftables_service_active=0
	firewalld_service_enabled=0
	ufw_service_enabled=0
	iptables_service_enabled=0
	nftables_service_enabled=0
	if command -v firewall-cmd >/dev/null 2>&1 && [ "$(firewall-cmd --state 2>/dev/null || true)" = "running" ]; then
		firewalld_active=1
	fi
	if command -v ufw >/dev/null 2>&1; then
		ufw_detection="${WORK_DIR}/ufw-detection"
		LC_ALL=C ufw status >"${ufw_detection}" 2>/dev/null || true
		grep -Eq '^Status:[[:space:]]+active$' "${ufw_detection}" && ufw_active=1
	fi
	if command -v awall >/dev/null 2>&1; then
		awall_detection="${WORK_DIR}/awall-detection"
		awall list >"${awall_detection}" 2>/dev/null || true
		awk '$2 == "enabled" || $2 == "required" { found = 1 } END { exit found ? 0 : 1 }' "${awall_detection}" && awall_configured=1
		if { is_root_safe_file /etc/iptables/rules-save && head -n 1 /etc/iptables/rules-save | grep -Fqx '# rules-save generated by awall'; } || \
			{ is_root_safe_file /etc/iptables/rules6-save && head -n 1 /etc/iptables/rules6-save | grep -Fqx '# rules6-save generated by awall'; }; then
			awall_owned_rules=1
		fi
		awall_live_rules="${WORK_DIR}/awall-live-iptables"
		if { [ "${awall_configured}" -eq 1 ] || [ "${awall_owned_rules}" -eq 1 ]; } && \
			live_iptables_restricts_input "${awall_live_rules}"; then
			awall_active=1
		elif [ "${awall_configured}" -eq 1 ] || [ "${awall_owned_rules}" -eq 1 ]; then
			log_line INFO "awall policy metadata exists but no live restrictive IPv4 INPUT rules were found; continuing with the active ruleset"
		fi
	fi
	command -v iptables >/dev/null 2>&1 && rc-service iptables status >/dev/null 2>&1 && iptables_service_active=1
	command -v nft >/dev/null 2>&1 && rc-service nftables status >/dev/null 2>&1 && nftables_service_active=1
	openrc_firewall_services="${WORK_DIR}/openrc-firewall-services"
	if ! rc-update show >"${openrc_firewall_services}" 2>>"${INSTALL_LOG}"; then
		log_line WARN "could not inspect OpenRC firewall enablement; automatic firewall changes are unsafe"
		[ "${FIREWALL_MODE}" = "open" ] && return 1
		return 0
	fi
	awk '$1 == "firewalld" && $2 == "|" { found = 1 } END { exit found ? 0 : 1 }' "${openrc_firewall_services}" && firewalld_service_enabled=1
	awk '$1 == "ufw" && $2 == "|" { found = 1 } END { exit found ? 0 : 1 }' "${openrc_firewall_services}" && ufw_service_enabled=1
	awk '$1 == "iptables" && $2 == "|" { found = 1 } END { exit found ? 0 : 1 }' "${openrc_firewall_services}" && iptables_service_enabled=1
	awk '$1 == "nftables" && $2 == "|" { found = 1 } END { exit found ? 0 : 1 }' "${openrc_firewall_services}" && nftables_service_enabled=1
	log_line INFO "OpenRC firewall services firewalld_active=${firewalld_active} firewalld_enabled=${firewalld_service_enabled} ufw_active=${ufw_active} ufw_enabled=${ufw_service_enabled} iptables_started=${iptables_service_active} iptables_enabled=${iptables_service_enabled} nftables_started=${nftables_service_active} nftables_enabled=${nftables_service_enabled}"
	if { [ "${firewalld_service_enabled}" -eq 1 ] && [ "${firewalld_active}" -eq 0 ]; } || \
		{ [ "${ufw_service_enabled}" -eq 1 ] && [ "${ufw_active}" -eq 0 ]; } || \
		{ [ "${iptables_service_enabled}" -eq 1 ] && [ "${iptables_service_active}" -eq 0 ]; } || \
		{ [ "${nftables_service_enabled}" -eq 1 ] && [ "${nftables_service_active}" -eq 0 ]; }; then
		log_line WARN "an OpenRC firewall service is enabled but not active; its next start may load rules that differ from the live ruleset"
		firewall_rule_warning "enabled but inactive OpenRC firewall service"
		[ "${FIREWALL_MODE}" = "open" ] && return 1
		return 0
	fi
	native_nft_input_active=0
	firewalld_nft_backend_active=0
	iptables_nft_input_hook_count=0
	nft_live_input_hook_count=0
	nft_live_audit_failed=0
	if command -v nft >/dev/null 2>&1; then
		firewall_owner_ruleset="${WORK_DIR}/firewall-owner-ruleset"
		if nft list ruleset >"${firewall_owner_ruleset}" 2>>"${INSTALL_LOG}"; then
			firewall_owner_ruleset_sanitized="${WORK_DIR}/firewall-owner-ruleset-sanitized"
			sed -E 's/comment "[^"]*"//g; s/# handle [0-9]+//g' "${firewall_owner_ruleset}" >"${firewall_owner_ruleset_sanitized}"
			nft_live_input_hook_count=$(grep -Ec 'type[[:space:]]+filter[[:space:]]+hook[[:space:]]+input([[:space:]]|;)' "${firewall_owner_ruleset_sanitized}" || true)
		else
			nft_live_audit_failed=1
		fi
		nft list chain inet filter input >/dev/null 2>&1 && native_nft_input_active=1
		nft list chain inet firewalld filter_INPUT >/dev/null 2>&1 && firewalld_nft_backend_active=1
		if nft list chain ip filter INPUT >/dev/null 2>&1; then
			iptables_nft_input_hook_count=$((iptables_nft_input_hook_count + 1))
		fi
		if nft list chain ip6 filter INPUT >/dev/null 2>&1; then
			iptables_nft_input_hook_count=$((iptables_nft_input_hook_count + 1))
		fi
	fi
	firewalld_raw_iptables_restrictive=0
	if [ "${firewalld_active}" -eq 1 ] && [ "${firewalld_nft_backend_active}" -eq 1 ] && command -v iptables-save >/dev/null 2>&1; then
		firewalld_raw_iptables="${WORK_DIR}/firewalld-raw-iptables"
		live_iptables_restricts_input "${firewalld_raw_iptables}" && firewalld_raw_iptables_restrictive=1
	fi
	log_line INFO "live firewall ownership nft_input_hooks=${nft_live_input_hook_count} iptables_nft_owned_hooks=${iptables_nft_input_hook_count} native_inet_filter_input=${native_nft_input_active} firewalld_nft_backend=${firewalld_nft_backend_active} restrictive_raw_iptables_with_firewalld=${firewalld_raw_iptables_restrictive}"
	active_frontends=$((firewalld_active + ufw_active + awall_active))
	if [ "${active_frontends}" -gt 1 ]; then
		log_line WARN "multiple firewall frontends are active (firewalld=${firewalld_active} ufw=${ufw_active} awall=${awall_active}); automatic changes are unsafe"
		firewall_rule_warning "multiple firewall managers"
		[ "${FIREWALL_MODE}" = "open" ] && return 1
		return 0
	fi
	independent_firewall_conflict=0
	if [ "${firewalld_active}" -eq 1 ] && { [ "${iptables_service_active}" -eq 1 ] || [ "${nftables_service_active}" -eq 1 ]; }; then
		independent_firewall_conflict=1
	fi
	if [ "${firewalld_active}" -eq 1 ]; then
		if [ "${nft_live_audit_failed}" -eq 1 ] || [ "${native_nft_input_active}" -eq 1 ] || \
			[ "${firewalld_raw_iptables_restrictive}" -eq 1 ] || \
			{ [ "${firewalld_nft_backend_active}" -eq 1 ] && [ "${nft_live_input_hook_count}" -ne 1 ]; } || \
			{ [ "${firewalld_nft_backend_active}" -eq 0 ] && [ "${nft_live_input_hook_count}" -ne 0 ]; }; then
			independent_firewall_conflict=1
		fi
	fi
	if { [ "${ufw_active}" -eq 1 ] || [ "${awall_active}" -eq 1 ]; } && [ "${nftables_service_active}" -eq 1 ]; then
		independent_firewall_conflict=1
	fi
	if { [ "${ufw_active}" -eq 1 ] || [ "${awall_active}" -eq 1 ]; } && \
		{ [ "${nft_live_audit_failed}" -eq 1 ] || [ "${nft_live_input_hook_count}" -ne "${iptables_nft_input_hook_count}" ]; }; then
		independent_firewall_conflict=1
	fi
	if [ "${independent_firewall_conflict}" -eq 1 ]; then
		log_line WARN "multiple independent firewall owners are active (firewalld=${firewalld_active} ufw=${ufw_active} awall=${awall_active} iptables_service=${iptables_service_active} nftables_service=${nftables_service_active} nft_input_hooks=${nft_live_input_hook_count} iptables_nft_owned_hooks=${iptables_nft_input_hook_count} native_inet_filter_input=${native_nft_input_active} restrictive_raw_iptables=${firewalld_raw_iptables_restrictive}); one backend allow rule cannot prove reachability across all INPUT hooks"
		firewall_rule_warning "multiple independent firewall owners"
		[ "${FIREWALL_MODE}" = "open" ] && return 1
		return 0
	fi
	if [ "${firewalld_active}" -eq 1 ]; then
		if ! configure_firewalld; then
			log_line WARN "firewalld access was not changed; inspect ${INSTALL_LOG}"
			[ "${FIREWALL_MODE}" = "open" ] && return 1
		fi
		return 0
	fi
	if [ "${ufw_active}" -eq 1 ]; then
		if ! configure_ufw; then
			log_line WARN "UFW access was not changed; inspect ${INSTALL_LOG}"
			[ "${FIREWALL_MODE}" = "open" ] && return 1
		fi
		return 0
	fi
	if [ "${awall_active}" -eq 1 ]; then
		firewall_rule_warning "awall"
		log_line WARN "awall policies are topology-specific; add a trusted-zone ${WEB_PORT}/tcp service rule and run 'awall activate' manually"
		[ "${FIREWALL_MODE}" = "open" ] && return 1
		return 0
	fi
	if [ "${iptables_service_active}" -eq 1 ] && [ "${nftables_service_active}" -eq 1 ]; then
		log_line WARN "both iptables and nftables OpenRC services are started; refusing to guess which service owns the INPUT firewall"
		firewall_rule_warning "simultaneous iptables and nftables services"
		[ "${FIREWALL_MODE}" = "open" ] && return 1
		return 0
	fi
	if [ "${iptables_service_active}" -eq 1 ]; then
		if command -v nft >/dev/null 2>&1 && nft list chain inet filter input >/dev/null 2>&1; then
			log_line WARN "iptables OpenRC is active alongside a native inet/filter/input chain; refusing to modify a mixed runtime ruleset"
			[ "${FIREWALL_MODE}" = "open" ] && return 1
			return 0
		fi
		if ! is_ipv4_literal "${WEB_HOST_LOWER}"; then
			log_line WARN "raw iptables only covers IPv4, but WebUI host=${WEB_HOST}; automatic access cannot be confirmed"
			[ "${FIREWALL_MODE}" = "open" ] && return 1
			return 0
		fi
		configure_standard_iptables
		iptables_config_rc=$?
		if [ "${iptables_config_rc}" -eq 0 ]; then
			return 0
		fi
		if [ "${iptables_config_rc}" -ne 2 ]; then
			log_line WARN "iptables access was not changed; inspect ${INSTALL_LOG}"
			[ "${FIREWALL_MODE}" = "open" ] && return 1
			return 0
		fi
	fi
	if command -v nft >/dev/null 2>&1; then
		nft_ruleset_file="${WORK_DIR}/nft-ruleset-detection"
		nft list ruleset >"${nft_ruleset_file}" 2>/dev/null || true
		if [ ! -s "${nft_ruleset_file}" ] && [ "${nftables_service_active}" -eq 1 ]; then
			log_line WARN "nftables OpenRC reports started but the live ruleset is empty; the next restart may restore a restrictive persistent policy"
			firewall_rule_warning "started nftables service with an empty live ruleset"
			[ "${FIREWALL_MODE}" = "open" ] && return 1
			return 0
		fi
		if [ -s "${nft_ruleset_file}" ]; then
			configure_standard_nftables
			nft_config_rc=$?
			if [ "${nft_config_rc}" -eq 0 ]; then
				return 0
			fi
			if [ "${nft_config_rc}" -ne 2 ]; then
				log_line WARN "nftables access was not changed; inspect ${INSTALL_LOG}"
				[ "${FIREWALL_MODE}" = "open" ] && return 1
				return 0
			fi
			firewall_rule_warning "custom nftables ruleset"
			log_line WARN "the active nftables ruleset has no standard inet/filter/input chain; manual review is required"
			[ "${FIREWALL_MODE}" = "open" ] && return 1
			return 0
		fi
	fi
	if command -v iptables >/dev/null 2>&1; then
		if ! is_ipv4_literal "${WEB_HOST_LOWER}"; then
			log_line WARN "raw iptables only covers IPv4, but WebUI host=${WEB_HOST}; automatic access cannot be confirmed"
			[ "${FIREWALL_MODE}" = "open" ] && return 1
			return 0
		fi
		configure_standard_iptables
		iptables_config_rc=$?
		if [ "${iptables_config_rc}" -eq 0 ]; then
			return 0
		fi
		if [ "${iptables_config_rc}" -ne 2 ]; then
			log_line WARN "iptables access was not changed; inspect ${INSTALL_LOG}"
			[ "${FIREWALL_MODE}" = "open" ] && return 1
			return 0
		fi
	fi
	log_line INFO "no active host INPUT firewall was detected; no local ${WEB_PORT}/tcp rule was added"
	log_line WARN "cloud security groups, upstream routers and carrier networks are outside this installer and may still block remote access"
	return 0
}

safe_remove_work_dir() {
	if [ -n "${WORK_DIR}" ]; then
		case "${WORK_DIR}" in
			/tmp/wwan-proxy-install.*) rm -rf -- "${WORK_DIR}" ;;
			*) log_line WARN "refusing to remove unexpected work directory: ${WORK_DIR}" ;;
		esac
	fi
}

collect_diagnostics() {
	diagnostic_reason=$1
	failed_step=${CURRENT_STEP}
	CURRENT_STEP="diagnostics"
	log_line ERROR "collecting diagnostics: ${diagnostic_reason}"
	diagnostic_file=${STEP_OUTPUT}
	if [ -z "${diagnostic_file}" ] || ! : >"${diagnostic_file}" 2>/dev/null; then
		diagnostic_file="/dev/null"
	fi
	{
		echo "===== diagnostics ${RUN_ID} ====="
		echo "reason=${diagnostic_reason}"
		echo "utc=$(timestamp)"
		echo "failed_step=${failed_step}"
		echo "alpine_release=$(cat /etc/alpine-release 2>/dev/null || echo unavailable)"
		echo "kernel=$(uname -a 2>&1)"
		echo "uid=$(id 2>&1)"
		echo "openrc=$(openrc --version 2>&1 | head -n 1)"
		echo "apk=$(apk --version 2>&1 | head -n 1)"
		echo "--- service status ---"
		if [ "${SERVICE_FILES_TRUSTED}" -eq 1 ]; then
			rc-service "${SERVICE_NAME}" status 2>&1 || true
		else
			echo "skipped: installed init/conf/binary files have not passed trust checks"
		fi
		echo "--- service registration ---"
		rc-update show 2>&1 | grep -F "${SERVICE_NAME}" || true
		echo "--- file metadata ---"
		ls -ld /usr/local/bin /etc/init.d /etc/conf.d "${DATA_DIR}" "${SERVICE_LOG_DIR}" 2>&1 || true
		ls -l "${INSTALL_BINARY}" "${INSTALL_INIT}" "${INSTALL_CONF}" "${DATABASE_PATH}" "${DATABASE_PATH}-wal" "${DATABASE_PATH}-shm" 2>&1 || true
		getcap "${INSTALL_BINARY}" 2>&1 || true
		if [ "${SERVICE_FILES_TRUSTED}" -eq 1 ]; then
			"${INSTALL_BINARY}" -version 2>&1 || true
		else
			echo "binary execution skipped: installed files are not trusted"
		fi
		echo "--- processes ---"
		ps -o pid,user,group,comm 2>&1 | grep -E '[w]wan-proxy|[s]upervise-daemon' || true
		service_pid=$(pidof wwan-proxy 2>/dev/null | awk '{print $1}')
		if [ -n "${service_pid}" ]; then
			grep -E '^(Max processes|Max open files)' "/proc/${service_pid}/limits" 2>&1 || true
			grep -E '^(Pid|Threads):' "/proc/${service_pid}/status" 2>&1 || true
		fi
		for pids_file in /sys/fs/cgroup/pids.current /sys/fs/cgroup/pids.max /sys/fs/cgroup/pids/pids.current /sys/fs/cgroup/pids/pids.max; do
			if [ -f "${pids_file}" ]; then
				echo "${pids_file} = $(cat "${pids_file}" 2>&1)"
			fi
		done
		echo "--- listening sockets ---"
		netstat -lntup 2>&1 || true
		echo "--- host firewall ---"
		if command -v firewall-cmd >/dev/null 2>&1; then
			firewall-cmd --state 2>&1 || true
			firewall-cmd --list-all 2>&1 || true
		fi
		if command -v ufw >/dev/null 2>&1; then
			LC_ALL=C ufw status verbose 2>&1 || true
		fi
		if command -v awall >/dev/null 2>&1; then
			awall list --all 2>&1 || true
		fi
		if command -v nft >/dev/null 2>&1; then
			nft -a list ruleset 2>&1 | head -n 500 || true
		fi
		if command -v iptables >/dev/null 2>&1; then
			iptables -S 2>&1 | head -n 500 || true
		fi
		echo "--- memory and filesystems ---"
		free -m 2>&1 || true
		df -h 2>&1 || true
		mount 2>&1 | sed -E 's/(password|passwd|credentials|username|user)=[^, )]+/\1=[redacted]/g' || true
		echo "--- DNS ---"
		grep -E '^[[:space:]]*(nameserver|search|options)[[:space:]]' /etc/resolv.conf 2>&1 || true
		nslookup github.com 2>&1 || true
		echo "--- interfaces and routes ---"
		if command -v ip >/dev/null 2>&1; then
			ip link show 2>&1 || true
			ip address show 2>&1 || true
			ip route show table all 2>&1 || true
			ip -6 route show table all 2>&1 || true
		else
			ifconfig -a 2>&1 || true
			route -n 2>&1 || true
		fi
		echo "--- recent service log ---"
		if [ "${SERVICE_LOG_TRUSTED}" -eq 1 ]; then
			tail -n 200 "${SERVICE_LOG}" 2>&1 || true
		else
			echo "skipped: service log path has not passed trust checks"
		fi
		echo "===== diagnostics end ====="
	} >"${diagnostic_file}" 2>&1
	append_output_to_log "diagnostics" "${diagnostic_file}"
	if [ "${diagnostic_file}" != "/dev/null" ]; then
		printf '%s\n' "----- failure diagnostics (last 240 lines) -----" >&2
		tail -n 240 "${diagnostic_file}" >&2 || true
		printf '%s\n' "----- end failure diagnostics -----" >&2
	fi
}

restore_file() {
	restore_had_file=$1
	restore_backup=$2
	restore_target=$3
	if [ "${restore_had_file}" -eq 1 ]; then
		cp -p "${restore_backup}" "${restore_target}" >>"${INSTALL_LOG}" 2>&1 || return 1
	else
		rm -f -- "${restore_target}" >>"${INSTALL_LOG}" 2>&1 || return 1
	fi
}

rollback_installation() {
	[ "${ROLLBACK_READY}" -eq 1 ] || return 0
	[ "${ROLLBACK_ACTIVE}" -eq 0 ] || return 0
	ROLLBACK_ACTIVE=1
	CURRENT_STEP="rollback"
	log_line WARN "rolling back program and OpenRC files; databases are preserved"
	rc-service "${SERVICE_NAME}" stop >>"${INSTALL_LOG}" 2>&1 || true
	rollback_failed=0
	restore_file "${HAD_BINARY}" "${WORK_DIR}/rollback/wwan-proxy" "${INSTALL_BINARY}" || rollback_failed=1
	restore_file "${HAD_INIT}" "${WORK_DIR}/rollback/wwan-proxy.openrc" "${INSTALL_INIT}" || rollback_failed=1
	restore_file "${HAD_CONF}" "${WORK_DIR}/rollback/wwan-proxy.confd" "${INSTALL_CONF}" || rollback_failed=1
	restore_file "${HAD_LOGROTATE}" "${WORK_DIR}/rollback/wwan-proxy.logrotate" "${INSTALL_LOGROTATE}" || rollback_failed=1
	if [ "${HAD_BINARY}" -eq 1 ] && [ -n "${OLD_BINARY_CAP}" ]; then
		setcap "${OLD_BINARY_CAP}" "${INSTALL_BINARY}" >>"${INSTALL_LOG}" 2>&1 || rollback_failed=1
	fi
	if [ "${PREVIOUS_ENABLED}" -eq 0 ]; then
		rc-update del "${SERVICE_NAME}" default >>"${INSTALL_LOG}" 2>&1 || true
	fi
	if [ "${PREVIOUS_RUNNING}" -eq 1 ] && [ "${HAD_BINARY}" -eq 1 ] && [ "${HAD_INIT}" -eq 1 ]; then
		if rc-service "${SERVICE_NAME}" start >>"${INSTALL_LOG}" 2>&1; then
			log_line WARN "previous service files restored and old service restarted"
		else
			rollback_failed=1
			log_line ERROR "old service files restored, but the previous service did not restart"
		fi
	fi
	if [ "${rollback_failed}" -ne 0 ]; then
		log_line ERROR "automatic rollback was incomplete; inspect ${INSTALL_LOG} and run: rc-service ${SERVICE_NAME} diagnose"
	else
		log_line WARN "automatic rollback completed"
	fi
	ROLLBACK_READY=0
}

on_exit() {
	exit_rc=$1
	trap - EXIT HUP INT TERM
	if [ "${exit_rc}" -ne 0 ] && [ "${SUCCESS}" -ne 1 ]; then
		rollback_firewall_transaction
		collect_diagnostics "installer exited with code ${exit_rc}"
		rollback_installation
	fi
	case "${BINARY_STAGE}" in
		/usr/local/bin/.wwan-proxy.install-*) rm -f -- "${BINARY_STAGE}" 2>/dev/null || true ;;
	esac
	case "${INIT_STAGE}" in
		/etc/init.d/.wwan-proxy.install-*) rm -f -- "${INIT_STAGE}" 2>/dev/null || true ;;
	esac
	safe_remove_work_dir
	if [ "${LOCK_HELD}" -eq 1 ]; then
		rm -f -- "${LOCK_DIR}/pid" 2>/dev/null || true
		rmdir "${LOCK_DIR}" 2>/dev/null || true
	fi
	if [ "${exit_rc}" -ne 0 ]; then
		CURRENT_STEP="failed"
		log_line ERROR "installation failed (exit=${exit_rc}); complete log: ${INSTALL_LOG}"
	fi
	exit "${exit_rc}"
}

trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM
trap 'on_exit $?' EXIT

if [ "$(id -u)" -ne 0 ]; then
	echo "${PROGRAM_NAME}: root privileges are required" >&2
	exit 1
fi

mkdir -p /var/log || { echo "${PROGRAM_NAME}: cannot create /var/log" >&2; exit 1; }
if [ -L "${INSTALL_LOG}" ] || { [ -e "${INSTALL_LOG}" ] && [ ! -f "${INSTALL_LOG}" ]; }; then
	echo "${PROGRAM_NAME}: refusing unsafe installer log target: ${INSTALL_LOG}" >&2
	exit 1
fi
touch "${INSTALL_LOG}" || { echo "${PROGRAM_NAME}: cannot write ${INSTALL_LOG}" >&2; exit 1; }
chmod 0600 "${INSTALL_LOG}" || true

CURRENT_STEP="bootstrap"
log_line INFO "===== wwan-proxy Alpine installer started ====="
log_line INFO "invocation pid=$$ user=$(id -un 2>/dev/null || echo unknown) repository=${REPOSITORY} requested_version=${RELEASE_VERSION}"

case "${REPOSITORY}" in
	*/*)
		repository_owner=${REPOSITORY%%/*}
		repository_name=${REPOSITORY#*/}
		case "${repository_owner}" in *[!A-Za-z0-9_.-]*|"") fatal "invalid GitHub repository owner: ${repository_owner}" ;; esac
		case "${repository_name}" in *[!A-Za-z0-9_.-]*|""|*/*) fatal "invalid GitHub repository name: ${repository_name}" ;; esac
		;;
	*) fatal "GitHub repository must use OWNER/REPO form: ${REPOSITORY}" ;;
esac
case "${RELEASE_VERSION}" in
	*[!A-Za-z0-9._-]*|"") fatal "invalid release tag: ${RELEASE_VERSION}" ;;
esac
case "${START_TIMEOUT}" in
	*[!0-9]*|"") fatal "--start-timeout must be an integer" ;;
esac
if [ "${START_TIMEOUT}" -lt 5 ] || [ "${START_TIMEOUT}" -gt 300 ]; then
	fatal "--start-timeout must be between 5 and 300 seconds"
fi
case "${FIREWALL_MODE}" in
	check|open|skip) ;;
	*) fatal "WWAN_PROXY_FIREWALL must be check, open, or skip" ;;
esac
if [ -n "${LOCAL_ARCHIVE}" ] && [ -z "${LOCAL_CHECKSUMS}" ]; then
	fatal "--archive requires --checksum; unverified local packages are not installed"
fi
if [ -z "${LOCAL_ARCHIVE}" ] && [ -n "${LOCAL_CHECKSUMS}" ]; then
	fatal "--checksum is only valid together with --archive"
fi
if [ -n "${HEALTH_URL}" ]; then
	case "${HEALTH_URL}" in
		http://*|https://*) ;;
		*) fatal "--health-url must start with http:// or https://" ;;
	esac
	case "${HEALTH_URL}" in
		*://*@*|*\?*|*\#*) fatal "health URL must not contain credentials, query parameters or fragments" ;;
	esac
fi

if ! mkdir "${LOCK_DIR}" 2>/dev/null; then
	lock_pid=$(cat "${LOCK_DIR}/pid" 2>/dev/null || echo unknown)
	case "${lock_pid}" in
		*[!0-9]*|"") lock_alive=0 ;;
		*)
			if kill -0 "${lock_pid}" 2>/dev/null; then lock_alive=1; else lock_alive=0; fi
			;;
	esac
	if [ "${lock_alive}" -eq 1 ]; then
		fatal "another installer is already running (lock=${LOCK_DIR}, pid=${lock_pid})"
	fi
	log_line WARN "removing stale installer lock (lock=${LOCK_DIR}, recorded_pid=${lock_pid})"
	rm -f -- "${LOCK_DIR}/pid" 2>>"${INSTALL_LOG}" || fatal "could not remove stale installer lock PID"
	rmdir "${LOCK_DIR}" 2>>"${INSTALL_LOG}" || fatal "stale installer lock contains unexpected files; inspect ${LOCK_DIR}"
	mkdir "${LOCK_DIR}" 2>>"${INSTALL_LOG}" || fatal "could not reacquire installer lock"
fi
LOCK_HELD=1
printf '%s\n' "$$" >"${LOCK_DIR}/pid"

WORK_DIR=$(mktemp -d /tmp/wwan-proxy-install.XXXXXX) || fatal "cannot create temporary work directory"
STEP_OUTPUT="${WORK_DIR}/step-output.log"
: >"${STEP_OUTPUT}"
mkdir -p "${WORK_DIR}/rollback" "${WORK_DIR}/extract" >>"${INSTALL_LOG}" 2>&1 || fatal "cannot initialize temporary workspace"
log_line DEBUG "work_directory=${WORK_DIR}"

if [ ! -r /etc/alpine-release ]; then
	if ! is_true "${FORCE_OS}"; then
		fatal "this installer requires Alpine Linux ${SUPPORTED_ALPINE_DISPLAY}; /etc/alpine-release is missing"
	fi
	ALPINE_RELEASE="unknown"
	log_line WARN "--force-os accepted a system without /etc/alpine-release"
else
	ALPINE_RELEASE=$(cat /etc/alpine-release)
	if ! is_supported_alpine_release "${ALPINE_RELEASE}"; then
		if is_true "${FORCE_OS}"; then
			log_line WARN "unsupported Alpine release ${ALPINE_RELEASE} accepted because --force-os was supplied"
		else
			fatal "Alpine ${ALPINE_RELEASE} is not supported; expected ${SUPPORTED_ALPINE_DISPLAY} (use --force-os only after testing)"
		fi
	fi
fi

if ! command -v apk >/dev/null 2>&1; then
	fatal "apk is unavailable; this is not an installable Alpine environment"
fi

APK_ARCH=$(apk --print-arch)
case "${APK_ARCH}" in
	x86_64)
		PACKAGE_NAME="wwan-proxy-linux-amd64-musl"
		;;
	aarch64)
		PACKAGE_NAME="wwan-proxy-linux-arm64-musl"
		;;
	*)
		fatal "unsupported Alpine architecture ${APK_ARCH}; available release assets are x86_64 and aarch64 musl"
		;;
esac
ASSET_NAME="${PACKAGE_NAME}.tar.gz"

CURRENT_STEP="system-inventory"
log_line INFO "Alpine=${ALPINE_RELEASE} kernel=$(uname -srmo 2>&1)"
log_line INFO "apk_arch=$(apk --print-arch 2>&1) uname_arch=$(uname -m 2>&1)"
log_line INFO "root_filesystem=$(df -P / 2>&1 | tail -n 1)"
log_line INFO "memory=$(free -m 2>&1 | awk '/^Mem:/ {print $2 "MiB total, " $4 "MiB free"}' || true)"
for proxy_name in HTTP_PROXY HTTPS_PROXY ALL_PROXY http_proxy https_proxy all_proxy; do
	proxy_present=0
	case "${proxy_name}" in
		HTTP_PROXY) [ "${HTTP_PROXY+x}" = x ] && proxy_present=1 ;;
		HTTPS_PROXY) [ "${HTTPS_PROXY+x}" = x ] && proxy_present=1 ;;
		ALL_PROXY) [ "${ALL_PROXY+x}" = x ] && proxy_present=1 ;;
		http_proxy) [ "${http_proxy+x}" = x ] && proxy_present=1 ;;
		https_proxy) [ "${https_proxy+x}" = x ] && proxy_present=1 ;;
		all_proxy) [ "${all_proxy+x}" = x ] && proxy_present=1 ;;
	esac
	log_line DEBUG "environment ${proxy_name}_set=${proxy_present} (value intentionally omitted)"
done
if [ -r /etc/apk/repositories ]; then
	while IFS= read -r repository_line || [ -n "${repository_line}" ]; do
		log_line DEBUG "apk_repository=${repository_line}"
	done </etc/apk/repositories
fi
if [ -r /etc/resolv.conf ]; then
	while IFS= read -r resolver_line || [ -n "${resolver_line}" ]; do
		case "${resolver_line}" in
			nameserver*|search*|options*) log_line DEBUG "resolver=${resolver_line}" ;;
		esac
	done </etc/resolv.conf
fi

set --
for dependency_package in ca-certificates curl libcap-utils logrotate openrc; do
	if ! apk info --exists "${dependency_package}" >/dev/null 2>&1; then
		set -- "$@" "${dependency_package}"
	fi
done
if [ "$#" -gt 0 ]; then
	run_step "install missing Alpine dependencies: $*" apk add --no-cache "$@" || \
		fatal "apk could not install required packages; check repositories, DNS and TLS in ${INSTALL_LOG}"
else
	log_line INFO "all required Alpine packages are already installed; apk network access skipped"
fi

for required_command in curl tar sha256sum setcap getcap rc-service rc-update openrc-run supervise-daemon; do
	if ! command -v "${required_command}" >/dev/null 2>&1; then
		fatal "required command is unavailable after apk install: ${required_command}"
	fi
	log_line DEBUG "command ${required_command}=$(command -v "${required_command}")"
done

download_file() {
	download_description=$1
	download_url=$2
	download_target=$3
	run_step "${download_description}" curl \
		--fail-with-body --location --show-error --silent \
		--retry 3 --retry-all-errors --retry-delay 2 \
		--connect-timeout 15 --max-time 300 \
		--output "${download_target}" \
		--write-out 'http_code=%{http_code} remote_ip=%{remote_ip} bytes=%{size_download} time=%{time_total}s effective_url=%{url_effective}\n' \
		"${download_url}"
}

if [ -n "${LOCAL_ARCHIVE}" ]; then
	[ -r "${LOCAL_ARCHIVE}" ] || fatal "local archive is not readable: ${LOCAL_ARCHIVE}"
	[ -r "${LOCAL_CHECKSUMS}" ] || fatal "local checksum file is not readable: ${LOCAL_CHECKSUMS}"
	ARCHIVE_PATH=${LOCAL_ARCHIVE}
	CHECKSUMS_PATH=${LOCAL_CHECKSUMS}
	ASSET_NAME=$(basename "${LOCAL_ARCHIVE}")
	log_line INFO "using local archive=${ARCHIVE_PATH} checksums=${CHECKSUMS_PATH}"
else
	if [ "${RELEASE_VERSION}" = "latest" ]; then
		RELEASE_METADATA="${WORK_DIR}/release.json"
		download_file "resolve latest GitHub release" \
			"https://api.github.com/repos/${REPOSITORY}/releases/latest" "${RELEASE_METADATA}" || \
			fatal "failed to resolve the latest release for ${REPOSITORY}"
		RELEASE_VERSION=$(sed -n 's/^[[:space:]]*"tag_name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' "${RELEASE_METADATA}" | head -n 1)
		if [ -z "${RELEASE_VERSION}" ]; then
			append_output_to_log "GitHub release metadata" "${RELEASE_METADATA}"
			fatal "GitHub response did not contain a release tag"
		fi
		case "${RELEASE_VERSION}" in
			*[!A-Za-z0-9._-]*) fatal "GitHub returned an unsafe release tag: ${RELEASE_VERSION}" ;;
		esac
	fi
	RELEASE_BASE="https://github.com/${REPOSITORY}/releases/download/${RELEASE_VERSION}"
	ARCHIVE_PATH="${WORK_DIR}/${ASSET_NAME}"
	CHECKSUMS_PATH="${WORK_DIR}/SHA256SUMS"
	log_line INFO "resolved release=${RELEASE_VERSION} asset=${ASSET_NAME}"
	download_file "download ${ASSET_NAME}" "${RELEASE_BASE}/${ASSET_NAME}" "${ARCHIVE_PATH}" || \
		fatal "failed to download ${ASSET_NAME}; the selected release may not provide this Alpine architecture"
	download_file "download SHA256SUMS" "${RELEASE_BASE}/SHA256SUMS" "${CHECKSUMS_PATH}" || \
		fatal "failed to download SHA256SUMS for release ${RELEASE_VERSION}"
fi

CHECKSUM_MATCHES="${WORK_DIR}/checksum-matches"
awk -v wanted="${ASSET_NAME}" '
{
	name=$2
	sub(/^\*/, "", name)
	sub(/^\.\//, "", name)
	if (name == wanted) print $1
}' "${CHECKSUMS_PATH}" >"${CHECKSUM_MATCHES}" || fatal "could not parse checksum file"
CHECKSUM_COUNT=$(wc -l <"${CHECKSUM_MATCHES}" | tr -d '[:space:]')
if [ "${CHECKSUM_COUNT}" -ne 1 ]; then
	fatal "SHA256SUMS must contain exactly one entry for ${ASSET_NAME}; found ${CHECKSUM_COUNT}"
fi
EXPECTED_SHA256=$(head -n 1 "${CHECKSUM_MATCHES}" | tr 'A-F' 'a-f')
case "${EXPECTED_SHA256}" in
	*[!0-9a-f]*|"") fatal "invalid SHA-256 value for ${ASSET_NAME}" ;;
esac
if [ "${#EXPECTED_SHA256}" -ne 64 ]; then
	fatal "invalid SHA-256 length for ${ASSET_NAME}"
fi
ACTUAL_SHA256=$(sha256sum "${ARCHIVE_PATH}" | awk '{print $1}') || fatal "could not hash ${ARCHIVE_PATH}"
log_line INFO "checksum expected=${EXPECTED_SHA256} actual=${ACTUAL_SHA256} asset=${ASSET_NAME}"
if [ "${EXPECTED_SHA256}" != "${ACTUAL_SHA256}" ]; then
	fatal "SHA-256 verification failed for ${ASSET_NAME}; archive was not installed"
fi

ARCHIVE_LIST="${WORK_DIR}/archive-list"
run_step "inspect verified release archive" tar -tzf "${ARCHIVE_PATH}" || fatal "release archive is unreadable"
tar -tzf "${ARCHIVE_PATH}" >"${ARCHIVE_LIST}" || fatal "could not list release archive"
if ! awk -F/ '
BEGIN { bad=0 }
substr($0, 1, 1) == "/" { bad=1 }
{
	for (i=1; i<=NF; i++) if ($i == "..") bad=1
}
END { exit bad }
' "${ARCHIVE_LIST}"; then
	fatal "release archive contains an unsafe absolute or parent path"
fi
TOP_DIRS="${WORK_DIR}/top-dirs"
awk -F/ 'NF > 0 && $1 != "." && $1 != "" { print $1 }' "${ARCHIVE_LIST}" | sort -u >"${TOP_DIRS}"
TOP_COUNT=$(wc -l <"${TOP_DIRS}" | tr -d '[:space:]')
if [ "${TOP_COUNT}" -ne 1 ]; then
	fatal "release archive must contain exactly one top-level directory; found ${TOP_COUNT}"
fi
TOP_DIR=$(head -n 1 "${TOP_DIRS}")
case "${TOP_DIR}" in
	*[!A-Za-z0-9._-]*|""|.|..) fatal "release archive has an unsafe top-level directory: ${TOP_DIR}" ;;
esac
run_step "extract verified release archive" tar -xzf "${ARCHIVE_PATH}" -C "${WORK_DIR}/extract" || fatal "release archive extraction failed"
PACKAGE_ROOT="${WORK_DIR}/extract/${TOP_DIR}"
ARCHIVE_LINKS=$(find "${PACKAGE_ROOT}" -type l -print 2>/dev/null || true)
if [ -n "${ARCHIVE_LINKS}" ]; then
	log_line ERROR "archive symlinks: ${ARCHIVE_LINKS}"
	fatal "release archive contains symbolic links; refusing to install"
fi

SOURCE_BINARY="${PACKAGE_ROOT}/wwan-proxy"
SOURCE_INIT="${PACKAGE_ROOT}/wwan-proxy.openrc"
SOURCE_CONF="${PACKAGE_ROOT}/wwan-proxy.confd"
SOURCE_LOGROTATE="${PACKAGE_ROOT}/wwan-proxy.logrotate"
for required_file in "${SOURCE_BINARY}" "${SOURCE_INIT}" "${SOURCE_CONF}" "${SOURCE_LOGROTATE}"; do
	[ -f "${required_file}" ] || fatal "release package is incomplete; missing ${required_file##*/}"
done
[ -x "${SOURCE_BINARY}" ] || fatal "release binary is not executable: ${SOURCE_BINARY}"
run_step "execute staged binary version check" "${SOURCE_BINARY}" -version || \
	fatal "release binary cannot execute on Alpine ${ALPINE_RELEASE}/${APK_ARCH}"
INSTALLED_VERSION=$("${SOURCE_BINARY}" -version 2>&1 | head -n 1)
log_line INFO "staged_binary_version=${INSTALLED_VERSION} staged_binary_sha256=$(sha256sum "${SOURCE_BINARY}" | awk '{print $1}')"

run_step "validate Alpine OpenRC script syntax" sh -n "${SOURCE_INIT}" || fatal "packaged OpenRC script has invalid shell syntax"

validate_fixed_conf_value() {
	conf_key=$1
	conf_expected=$2
	conf_matches="${WORK_DIR}/conf-${conf_key}"
	sed -n "s/^[[:space:]]*${conf_key}[[:space:]]*=[[:space:]]*//p" "${INSTALL_CONF}" >"${conf_matches}" || \
		fatal "could not inspect ${conf_key} in ${INSTALL_CONF}"
	conf_count=$(wc -l <"${conf_matches}" | tr -d '[:space:]')
	if [ "${conf_count}" -gt 1 ]; then
		fatal "${INSTALL_CONF} defines ${conf_key} more than once"
	fi
	[ "${conf_count}" -eq 1 ] || return 0
	conf_value=$(head -n 1 "${conf_matches}" | sed 's/[[:space:]]*$//')
	case "${conf_value}" in
		\"*\") conf_value=${conf_value#\"}; conf_value=${conf_value%\"} ;;
		\'*\') conf_value=${conf_value#\'}; conf_value=${conf_value%\'} ;;
	esac
	if [ "${conf_value}" != "${conf_expected}" ]; then
		fatal "${conf_key} in ${INSTALL_CONF} must remain ${conf_expected}; the installer manages this fixed path/account"
	fi
}

for protected_target in \
	"${INSTALL_BINARY}" "${INSTALL_INIT}" "${INSTALL_CONF}" "${INSTALL_LOGROTATE}" \
	"${DATABASE_PATH}" "${DATABASE_PATH}-wal" "${DATABASE_PATH}-shm" \
	"${DATA_DIR}" "${SERVICE_LOG_DIR}" "${SERVICE_LOG}" /run/wwan-proxy "${BACKUP_ROOT}"
do
	if [ -L "${protected_target}" ]; then
		fatal "refusing to replace symbolic-link target: ${protected_target}"
	fi
done

assert_root_controlled_file "${INSTALL_BINARY}" "installed binary"
assert_root_controlled_file "${INSTALL_INIT}" "OpenRC init script"
assert_root_controlled_file "${INSTALL_CONF}" "OpenRC configuration"
SERVICE_FILES_TRUSTED=1

if [ -e "${INSTALL_CONF}" ]; then
	for fixed_conf_pair in \
		"WWAN_PROXY_USER=wwan-proxy" \
		"WWAN_PROXY_GROUP=wwan-proxy" \
		"WWAN_PROXY_BINARY=${INSTALL_BINARY}" \
		"WWAN_PROXY_DATA_DIR=${DATA_DIR}" \
		"WWAN_PROXY_DATABASE=${DATABASE_PATH}" \
		"WWAN_PROXY_LOG_DIR=${SERVICE_LOG_DIR}" \
		"WWAN_PROXY_RUNTIME_DIR=/run/wwan-proxy"
	do
		validate_fixed_conf_value "${fixed_conf_pair%%=*}" "${fixed_conf_pair#*=}"
	done
fi

if rc-service "${SERVICE_NAME}" status >/dev/null 2>&1; then PREVIOUS_RUNNING=1; fi
if rc-update show default 2>/dev/null | grep -Eq "(^|[[:space:]])${SERVICE_NAME}([[:space:]]|$)"; then PREVIOUS_ENABLED=1; fi
[ -f "${INSTALL_BINARY}" ] && HAD_BINARY=1
[ -f "${INSTALL_INIT}" ] && HAD_INIT=1
[ -f "${INSTALL_CONF}" ] && HAD_CONF=1
[ -f "${INSTALL_LOGROTATE}" ] && HAD_LOGROTATE=1
[ -f "${DATABASE_PATH}" ] && HAD_DATABASE=1
log_line INFO "previous_state running=${PREVIOUS_RUNNING} enabled=${PREVIOUS_ENABLED} binary=${HAD_BINARY} database=${HAD_DATABASE}"

if [ "${HAD_BINARY}" -eq 1 ]; then
	cp -p "${INSTALL_BINARY}" "${WORK_DIR}/rollback/wwan-proxy" >>"${INSTALL_LOG}" 2>&1 || fatal "could not back up installed binary"
	OLD_BINARY_CAP=$(getcap "${INSTALL_BINARY}" 2>/dev/null | sed -n 's/^[^ ]*[[:space:]]*//p' | head -n 1)
fi
if [ "${HAD_INIT}" -eq 1 ]; then
	cp -p "${INSTALL_INIT}" "${WORK_DIR}/rollback/wwan-proxy.openrc" >>"${INSTALL_LOG}" 2>&1 || fatal "could not back up OpenRC script"
fi
if [ "${HAD_CONF}" -eq 1 ]; then
	cp -p "${INSTALL_CONF}" "${WORK_DIR}/rollback/wwan-proxy.confd" >>"${INSTALL_LOG}" 2>&1 || fatal "could not back up OpenRC configuration"
fi
if [ "${HAD_LOGROTATE}" -eq 1 ]; then
	cp -p "${INSTALL_LOGROTATE}" "${WORK_DIR}/rollback/wwan-proxy.logrotate" >>"${INSTALL_LOG}" 2>&1 || fatal "could not back up logrotate configuration"
fi
ROLLBACK_READY=1

if ! grep -q '^wwan-proxy:' /etc/group 2>/dev/null; then
	run_step "create wwan-proxy system group" addgroup -S wwan-proxy || fatal "could not create service group"
fi
if ! grep -q '^wwan-proxy:' /etc/passwd 2>/dev/null; then
	run_step "create wwan-proxy system user" adduser -S -D -H -h "${DATA_DIR}" -s /sbin/nologin -G wwan-proxy wwan-proxy || \
		fatal "could not create service user"
fi
SERVICE_UID=$(awk -F: '$1 == "wwan-proxy" { print $3; exit }' /etc/passwd)
case "${SERVICE_UID}" in
	""|*[!0-9]*) fatal "service user has an invalid UID" ;;
	0) fatal "refusing to run wwan-proxy with UID 0" ;;
esac

run_step "create persistent data, log, runtime and backup directories" \
	mkdir -p "${DATA_DIR}" "${SERVICE_LOG_DIR}" /run/wwan-proxy "${BACKUP_ROOT}" || fatal "could not create service directories"
run_step "set data directory ownership" chown wwan-proxy:wwan-proxy "${DATA_DIR}" || fatal "could not set data directory ownership"
run_step "secure service log directory ownership" chown root:wwan-proxy "${SERVICE_LOG_DIR}" || fatal "could not secure service log directory ownership"
run_step "set service directory permissions" chmod 0750 "${DATA_DIR}" "${SERVICE_LOG_DIR}" || fatal "could not set directory permissions"
chown root:root /run/wwan-proxy "${BACKUP_ROOT}" >>"${INSTALL_LOG}" 2>&1 || fatal "could not secure runtime or backup directory"
chmod 0755 /run/wwan-proxy >>"${INSTALL_LOG}" 2>&1 || fatal "could not set runtime directory permissions"
chmod 0700 "${BACKUP_ROOT}" >>"${INSTALL_LOG}" 2>&1 || fatal "could not set backup directory permissions"
if [ -L "${SERVICE_LOG}" ] || { [ -e "${SERVICE_LOG}" ] && [ ! -f "${SERVICE_LOG}" ]; }; then
	fatal "service log must be a regular file and not a symbolic link: ${SERVICE_LOG}"
fi
touch "${SERVICE_LOG}" >>"${INSTALL_LOG}" 2>&1 || fatal "could not create service log"
chown wwan-proxy:wwan-proxy "${SERVICE_LOG}" >>"${INSTALL_LOG}" 2>&1 || fatal "could not set service log ownership"
chmod 0640 "${SERVICE_LOG}" >>"${INSTALL_LOG}" 2>&1 || fatal "could not set service log permissions"
SERVICE_LOG_METADATA=$(stat -c '%U:%G:%a' "${SERVICE_LOG}" 2>/dev/null || echo unknown)
[ "${SERVICE_LOG_METADATA}" = "wwan-proxy:wwan-proxy:640" ] || fatal "service log metadata verification failed: ${SERVICE_LOG_METADATA}"
SERVICE_LOG_TRUSTED=1
for database_file in "${DATABASE_PATH}" "${DATABASE_PATH}-wal" "${DATABASE_PATH}-shm"; do
	if [ -f "${database_file}" ]; then
		chown wwan-proxy:wwan-proxy "${database_file}" >>"${INSTALL_LOG}" 2>&1 || fatal "could not set ownership on ${database_file}"
		chmod 0600 "${database_file}" >>"${INSTALL_LOG}" 2>&1 || fatal "could not set permissions on ${database_file}"
	fi
done

BINARY_CHANGED=1
INIT_CHANGED=1
if [ "${HAD_BINARY}" -eq 1 ] && cmp -s "${SOURCE_BINARY}" "${INSTALL_BINARY}"; then BINARY_CHANGED=0; fi
if [ "${HAD_INIT}" -eq 1 ] && cmp -s "${SOURCE_INIT}" "${INSTALL_INIT}"; then INIT_CHANGED=0; fi

CAPABILITY_OK=0
if [ "${HAD_BINARY}" -eq 1 ] && getcap "${INSTALL_BINARY}" 2>/dev/null | grep -Eq '(^|[[:space:]])cap_net_raw([=,+]|,)'; then
	CAPABILITY_OK=1
fi
BINARY_METADATA_OK=0
if [ "${HAD_BINARY}" -eq 1 ]; then
	BINARY_METADATA=$(stat -c '%U:%G:%a' "${INSTALL_BINARY}" 2>/dev/null || echo unknown)
	[ "${BINARY_METADATA}" = "root:wwan-proxy:750" ] && BINARY_METADATA_OK=1
fi
if [ "${BINARY_CHANGED}" -eq 1 ] || [ "${INIT_CHANGED}" -eq 1 ] || [ "${CAPABILITY_OK}" -eq 0 ] || [ "${BINARY_METADATA_OK}" -eq 0 ]; then
	FILES_REQUIRE_RESTART=1
fi
log_line INFO "change_plan binary_changed=${BINARY_CHANGED} init_changed=${INIT_CHANGED} capability_ok=${CAPABILITY_OK} metadata_ok=${BINARY_METADATA_OK}"

if [ "${PREVIOUS_RUNNING}" -eq 1 ] && [ "${FILES_REQUIRE_RESTART}" -eq 1 ]; then
	run_step "stop existing wwan-proxy service" rc-service "${SERVICE_NAME}" stop || fatal "could not stop the existing service safely"
fi

if [ "${HAD_DATABASE}" -eq 1 ] && { [ "${PREVIOUS_RUNNING}" -eq 0 ] || [ "${FILES_REQUIRE_RESTART}" -eq 1 ]; }; then
	BACKUP_DIR="${BACKUP_ROOT}/install-${RUN_ID}"
	mkdir -p "${BACKUP_DIR}" >>"${INSTALL_LOG}" 2>&1 || fatal "could not create database safety backup directory"
	chmod 0700 "${BACKUP_DIR}" >>"${INSTALL_LOG}" 2>&1 || fatal "could not secure database safety backup directory"
	for database_file in "${DATABASE_PATH}" "${DATABASE_PATH}-wal" "${DATABASE_PATH}-shm"; do
		if [ -f "${database_file}" ]; then
			cp -p "${database_file}" "${BACKUP_DIR}/" >>"${INSTALL_LOG}" 2>&1 || fatal "could not back up ${database_file}"
		fi
	done
	log_line INFO "bootstrap database safety copy=${BACKUP_DIR} (not modified during automatic rollback)"
fi

if [ "${BINARY_CHANGED}" -eq 1 ]; then
	BINARY_STAGE="/usr/local/bin/.wwan-proxy.install-${RUN_ID}"
	cp "${SOURCE_BINARY}" "${BINARY_STAGE}" >>"${INSTALL_LOG}" 2>&1 || fatal "could not stage the new binary"
	chown root:wwan-proxy "${BINARY_STAGE}" >>"${INSTALL_LOG}" 2>&1 || fatal "could not set staged binary ownership"
	chmod 0750 "${BINARY_STAGE}" >>"${INSTALL_LOG}" 2>&1 || fatal "could not set staged binary permissions"
	setcap cap_net_raw=ep "${BINARY_STAGE}" >>"${INSTALL_LOG}" 2>&1 || fatal "setcap failed; check filesystem xattr support and CAP_SETFCAP"
	getcap "${BINARY_STAGE}" | grep -Eq '(^|[[:space:]])cap_net_raw([=,+]|,)' || fatal "cap_net_raw verification failed on staged binary"
	mv -f "${BINARY_STAGE}" "${INSTALL_BINARY}" >>"${INSTALL_LOG}" 2>&1 || fatal "could not atomically publish the new binary"
else
	chown root:wwan-proxy "${INSTALL_BINARY}" >>"${INSTALL_LOG}" 2>&1 || fatal "could not repair installed binary ownership"
	chmod 0750 "${INSTALL_BINARY}" >>"${INSTALL_LOG}" 2>&1 || fatal "could not repair installed binary permissions"
	setcap cap_net_raw=ep "${INSTALL_BINARY}" >>"${INSTALL_LOG}" 2>&1 || fatal "could not repair cap_net_raw on installed binary"
fi
getcap "${INSTALL_BINARY}" | grep -Eq '(^|[[:space:]])cap_net_raw([=,+]|,)' || fatal "installed binary is missing cap_net_raw"
run_step "execute installed binary version check" "${INSTALL_BINARY}" -version || fatal "installed binary preflight failed"

if [ "${INIT_CHANGED}" -eq 1 ]; then
	INIT_STAGE="/etc/init.d/.wwan-proxy.install-${RUN_ID}"
	cp "${SOURCE_INIT}" "${INIT_STAGE}" >>"${INSTALL_LOG}" 2>&1 || fatal "could not stage OpenRC script"
	chown root:root "${INIT_STAGE}" >>"${INSTALL_LOG}" 2>&1 || fatal "could not set OpenRC script ownership"
	chmod 0755 "${INIT_STAGE}" >>"${INSTALL_LOG}" 2>&1 || fatal "could not set OpenRC script permissions"
	mv -f "${INIT_STAGE}" "${INSTALL_INIT}" >>"${INSTALL_LOG}" 2>&1 || fatal "could not publish OpenRC script"
else
	chown root:root "${INSTALL_INIT}" >>"${INSTALL_LOG}" 2>&1 || fatal "could not repair OpenRC script ownership"
	chmod 0755 "${INSTALL_INIT}" >>"${INSTALL_LOG}" 2>&1 || fatal "could not repair OpenRC script permissions"
fi

if [ ! -e "${INSTALL_CONF}" ]; then
	cp "${SOURCE_CONF}" "${INSTALL_CONF}" >>"${INSTALL_LOG}" 2>&1 || fatal "could not install OpenRC configuration"
	log_line INFO "created ${INSTALL_CONF}"
else
	log_line INFO "preserved existing ${INSTALL_CONF}"
fi
chown root:wwan-proxy "${INSTALL_CONF}" >>"${INSTALL_LOG}" 2>&1 || fatal "could not set OpenRC configuration ownership"
chmod 0640 "${INSTALL_CONF}" >>"${INSTALL_LOG}" 2>&1 || fatal "could not set OpenRC configuration permissions"

LOGROTATE_CHANGED=1
if [ "${HAD_LOGROTATE}" -eq 1 ] && cmp -s "${SOURCE_LOGROTATE}" "${INSTALL_LOGROTATE}"; then LOGROTATE_CHANGED=0; fi
if [ "${LOGROTATE_CHANGED}" -eq 1 ]; then
	cp "${SOURCE_LOGROTATE}" "${INSTALL_LOGROTATE}" >>"${INSTALL_LOG}" 2>&1 || fatal "could not install logrotate configuration"
fi
chown root:root "${INSTALL_LOGROTATE}" >>"${INSTALL_LOG}" 2>&1 || fatal "could not set logrotate configuration ownership"
chmod 0644 "${INSTALL_LOGROTATE}" >>"${INSTALL_LOG}" 2>&1 || fatal "could not set logrotate configuration permissions"
run_step "validate logrotate configuration" logrotate --debug "${INSTALL_LOGROTATE}" || fatal "installed logrotate configuration is invalid"

run_step "validate final OpenRC installation" rc-service "${SERVICE_NAME}" check || fatal "OpenRC installation check failed"

read_configured_health_url() {
	CONFIGURED_HEALTH_URL=""
	configured_health_matches="${WORK_DIR}/configured-health-urls"
	sed -n 's/^[[:space:]]*WWAN_PROXY_HEALTH_URL[[:space:]]*=[[:space:]]*//p' "${INSTALL_CONF}" >"${configured_health_matches}" || return 1
	configured_health_count=$(wc -l <"${configured_health_matches}" | tr -d '[:space:]')
	[ "${configured_health_count}" -le 1 ] || return 1
	[ "${configured_health_count}" -eq 1 ] || return 0
	configured_line=$(head -n 1 "${configured_health_matches}" | sed 's/[[:space:]]*$//')
	case "${configured_line}" in
		\"*\") configured_line=${configured_line#\"}; configured_line=${configured_line%\"} ;;
		\'*\') configured_line=${configured_line#\'}; configured_line=${configured_line%\'} ;;
	esac
	case "${configured_line}" in
		http://*|https://*) CONFIGURED_HEALTH_URL=${configured_line} ;;
		*) return 1 ;;
	esac
	case "${CONFIGURED_HEALTH_URL}" in
		*://*@*|*\?*|*\#*) return 1 ;;
	esac
}

if [ -z "${HEALTH_URL}" ]; then
	read_configured_health_url || fatal "${INSTALL_CONF} contains an invalid or ambiguous WWAN_PROXY_HEALTH_URL"
	HEALTH_URL=${CONFIGURED_HEALTH_URL}
	if [ -n "${HEALTH_URL}" ] && [ "${HEALTH_URL}" != "${DEFAULT_HEALTH_URL}" ]; then
		HEALTH_URL_EXPLICIT=1
	fi
fi
[ -n "${HEALTH_URL}" ] || HEALTH_URL=${DEFAULT_HEALTH_URL}
HEALTH_DISPLAY=$(printf '%s' "${HEALTH_URL}" | sed -E 's#(https?://)[^/@]+@#\1[redacted]@#; s#[?#].*$##')

if is_true "${NO_START}"; then
	CURRENT_STEP="complete"
	ROLLBACK_READY=0
	SUCCESS=1
	log_line WARN "--no-start selected; service was not enabled or started, and firewall inspection or changes were deferred"
	log_line INFO "installed version=${INSTALLED_VERSION} binary=${INSTALL_BINARY}"
	log_line INFO "enable later: rc-update add ${SERVICE_NAME} default && rc-service ${SERVICE_NAME} start"
	log_line INFO "complete log: ${INSTALL_LOG}"
	exit 0
fi

if [ "${PREVIOUS_ENABLED}" -eq 0 ]; then
	run_step "enable wwan-proxy in the default OpenRC runlevel" rc-update add "${SERVICE_NAME}" default || fatal "could not enable the OpenRC service"
fi

if [ "${PREVIOUS_RUNNING}" -eq 1 ] && [ "${FILES_REQUIRE_RESTART}" -eq 0 ]; then
	log_line INFO "installed files are unchanged and the service is already running; restart skipped"
elif rc-service "${SERVICE_NAME}" status >/dev/null 2>&1; then
	run_step "restart wwan-proxy service" rc-service "${SERVICE_NAME}" restart || fatal "OpenRC could not restart wwan-proxy"
else
	run_step "start wwan-proxy service" rc-service "${SERVICE_NAME}" start || fatal "OpenRC could not start wwan-proxy"
fi

CURRENT_STEP="service-stability"
stable_seconds=0
while [ "${stable_seconds}" -lt 5 ]; do
	if ! rc-service "${SERVICE_NAME}" status >"${STEP_OUTPUT}" 2>&1; then
		append_output_to_log "service status" "${STEP_OUTPUT}"
		fatal "wwan-proxy did not remain running during the stability window"
	fi
	append_output_to_log "service status" "${STEP_OUTPUT}"
	stable_seconds=$((stable_seconds + 1))
	[ "${stable_seconds}" -ge 5 ] || sleep 1
done
log_line INFO "OpenRC service remained running for ${stable_seconds} consecutive checks"

SERVICE_PID=$(pidof wwan-proxy 2>/dev/null | awk '{print $1}')
[ -n "${SERVICE_PID}" ] || fatal "could not identify the running wwan-proxy process"
SERVICE_NPROC_LIMIT=$(awk '$1 == "Max" && $2 == "processes" { print $3 "/" $4; exit }' "/proc/${SERVICE_PID}/limits")
SERVICE_NOFILE_LIMIT=$(awk '$1 == "Max" && $2 == "open" && $3 == "files" { print $4 "/" $5; exit }' "/proc/${SERVICE_PID}/limits")
log_line INFO "service_limits pid=${SERVICE_PID} nproc_soft_hard=${SERVICE_NPROC_LIMIT:-unknown} nofile_soft_hard=${SERVICE_NOFILE_LIMIT:-unknown}"
if [ -f /sys/fs/cgroup/pids.max ]; then
	CGROUP_PIDS_MAX=$(cat /sys/fs/cgroup/pids.max 2>/dev/null || echo unknown)
	log_line INFO "cgroup_pids_max=${CGROUP_PIDS_MAX} (independent of service RLIMIT_NPROC)"
elif [ -f /sys/fs/cgroup/pids/pids.max ]; then
	CGROUP_PIDS_MAX=$(cat /sys/fs/cgroup/pids/pids.max 2>/dev/null || echo unknown)
	log_line INFO "cgroup_pids_max=${CGROUP_PIDS_MAX} (independent of service RLIMIT_NPROC)"
fi

CURRENT_STEP="health-check"
health_started=$(date +%s)
health_deadline=$((health_started + START_TIMEOUT))
health_attempt=0
health_ok=0
while [ "$(date +%s)" -lt "${health_deadline}" ]; do
	if ! rc-service "${SERVICE_NAME}" status >/dev/null 2>&1; then
		fatal "wwan-proxy stopped while waiting for the health endpoint"
	fi
	health_attempt=$((health_attempt + 1))
	HEALTH_BODY="${WORK_DIR}/health-body"
	HEALTH_ERROR="${WORK_DIR}/health-error"
	if curl --fail --show-error --silent --connect-timeout 2 --max-time 3 --output "${HEALTH_BODY}" "${HEALTH_URL}" 2>"${HEALTH_ERROR}"; then
		if grep -Eq '"ok"[[:space:]]*:[[:space:]]*true' "${HEALTH_BODY}"; then
			health_ok=1
			break
		fi
		printf '%s\n' "health response did not contain ok=true: $(head -c 500 "${HEALTH_BODY}" 2>/dev/null)" >"${HEALTH_ERROR}"
	fi
	append_output_to_log "health attempt ${health_attempt}" "${HEALTH_ERROR}"
	[ "$(date +%s)" -ge "${health_deadline}" ] || sleep 1
done
health_finished=$(date +%s)
health_elapsed=$((health_finished - health_started))

if [ "${health_ok}" -eq 0 ]; then
	if [ "${HAD_DATABASE}" -eq 1 ] && [ "${HEALTH_URL_EXPLICIT}" -eq 0 ]; then
		log_line WARN "service is stable but ${HEALTH_DISPLAY} did not answer; an existing database may use a different WebUI listen address"
		log_line WARN "set WWAN_PROXY_HEALTH_URL in ${INSTALL_CONF} or rerun with --health-url to verify HTTP explicitly"
	else
		fatal "health check failed after ${health_elapsed}s (${health_attempt} attempts): ${HEALTH_DISPLAY}"
	fi
else
	log_line INFO "health check passed after ${health_elapsed}s (${health_attempt} attempts): ${HEALTH_DISPLAY}"
fi

if ! configure_host_firewall; then
	# The program and service are healthy; preserve them so the administrator can
	# apply the logged manual rule without reinstalling, but do not report a
	# remotely reachable installation when the requested open mode was not met.
	ROLLBACK_READY=0
	fatal "service installation succeeded, but remote WebUI firewall access could not be confirmed; the service was kept running and ${INSTALL_LOG} contains the required manual action"
fi

CURRENT_STEP="complete"
ROLLBACK_READY=0
SUCCESS=1
log_line INFO "installation successful version=${INSTALLED_VERSION} release=${RELEASE_VERSION} architecture=${APK_ARCH}"
log_line INFO "binary=${INSTALL_BINARY} database=${DATABASE_PATH} service_log=${SERVICE_LOG}"
[ -n "${WEB_LISTEN:-}" ] && log_line INFO "WebUI listener=${WEB_LISTEN} remote_url=http://<Alpine-host-IP>:${WEB_PORT}"
[ -n "${BACKUP_DIR}" ] && log_line INFO "pre-upgrade bootstrap database copy=${BACKUP_DIR}"
log_line INFO "diagnose: rc-service ${SERVICE_NAME} diagnose"
log_line INFO "follow logs: rc-service ${SERVICE_NAME} follow"
log_line INFO "complete installer log: ${INSTALL_LOG}"
log_line INFO "===== wwan-proxy Alpine installer completed ====="

exit 0

#!/bin/sh

# One-click installer for wwan-proxy on Alpine Linux 3.23.
# BusyBox ash compatible: intentionally avoids Bash-only syntax.
# ShellCheck cannot see trap callbacks and pgrep is not guaranteed in BusyBox.
# shellcheck disable=SC2009,SC2329

set -u

PROGRAM_NAME="install-alpine.sh"
SERVICE_NAME="wwan-proxy"
DEFAULT_REPOSITORY="iskycc/wwan-proxy"
SUPPORTED_ALPINE_SERIES="3.23"
DEFAULT_HEALTH_URL="http://127.0.0.1:9090/api/health"

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

usage() {
	cat <<'EOF'
Usage:
  install-alpine.sh [options]

Install or upgrade wwan-proxy on Alpine Linux 3.23 using a verified musl
release package and an OpenRC service.

Options:
  --version TAG          Release tag to install (default: latest)
  --repo OWNER/REPO      GitHub repository (default: iskycc/wwan-proxy)
  --archive FILE         Install a local release archive
  --checksum FILE        SHA256SUMS for --archive (required with --archive)
  --health-url URL       Post-start health endpoint
  --start-timeout SEC    Start/health timeout, 5-300 seconds (default: 30)
  --no-start             Install files without enabling or starting OpenRC
  --force-os             Allow an Alpine release other than 3.23
  -h, --help             Show this help

Environment equivalents:
  WWAN_PROXY_VERSION, WWAN_PROXY_REPOSITORY, WWAN_PROXY_ARCHIVE,
  WWAN_PROXY_CHECKSUMS, WWAN_PROXY_HEALTH_URL, WWAN_PROXY_START_TIMEOUT,
  WWAN_PROXY_NO_START, WWAN_PROXY_FORCE_OS

Examples:
  curl -fsSL https://raw.githubusercontent.com/iskycc/wwan-proxy/main/scripts/install-alpine.sh | sh
  sh install-alpine.sh --version build-0123456789ab
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
		echo "--- listening sockets ---"
		netstat -lntup 2>&1 || true
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
		fatal "this installer requires Alpine Linux ${SUPPORTED_ALPINE_SERIES}.x; /etc/alpine-release is missing"
	fi
	ALPINE_RELEASE="unknown"
	log_line WARN "--force-os accepted a system without /etc/alpine-release"
else
	ALPINE_RELEASE=$(cat /etc/alpine-release)
	case "${ALPINE_RELEASE}" in
		${SUPPORTED_ALPINE_SERIES}.*) ;;
		*)
			if is_true "${FORCE_OS}"; then
				log_line WARN "unsupported Alpine release ${ALPINE_RELEASE} accepted because --force-os was supplied"
			else
				fatal "Alpine ${ALPINE_RELEASE} is not supported; expected ${SUPPORTED_ALPINE_SERIES}.x (use --force-os only after testing)"
			fi
			;;
	esac
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
	log_line WARN "--no-start selected; service was not enabled or started"
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

CURRENT_STEP="complete"
ROLLBACK_READY=0
SUCCESS=1
log_line INFO "installation successful version=${INSTALLED_VERSION} release=${RELEASE_VERSION} architecture=${APK_ARCH}"
log_line INFO "binary=${INSTALL_BINARY} database=${DATABASE_PATH} service_log=${SERVICE_LOG}"
[ -n "${BACKUP_DIR}" ] && log_line INFO "pre-upgrade bootstrap database copy=${BACKUP_DIR}"
log_line INFO "diagnose: rc-service ${SERVICE_NAME} diagnose"
log_line INFO "follow logs: rc-service ${SERVICE_NAME} follow"
log_line INFO "complete installer log: ${INSTALL_LOG}"
log_line INFO "===== wwan-proxy Alpine installer completed ====="

exit 0

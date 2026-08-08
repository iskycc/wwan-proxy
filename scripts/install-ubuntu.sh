#!/usr/bin/env bash

# One-click installer for wwan-proxy on Ubuntu systems using systemd.

set -Eeuo pipefail

PROGRAM_NAME="install-ubuntu.sh"
SERVICE_NAME="wwan-proxy"
DEFAULT_REPOSITORY="iskycc/wwan-proxy"
SUPPORTED_UBUNTU_LTS="22.04 24.04 26.04"

INSTALL_BINARY="/usr/local/bin/wwan-proxy"
INSTALL_UNIT="/etc/systemd/system/wwan-proxy.service"
DATA_DIR="/var/lib/wwan-proxy"
DATABASE_PATH="${DATA_DIR}/wwan-proxy.db"
INSTALL_LOG="/var/log/wwan-proxy-install.log"
BACKUP_ROOT="/var/backups/wwan-proxy"
LOCK_DIR="/run/wwan-proxy-ubuntu-install.lock"

REPOSITORY="${WWAN_PROXY_REPOSITORY:-${DEFAULT_REPOSITORY}}"
RELEASE_VERSION="${WWAN_PROXY_VERSION:-latest}"
LOCAL_ARCHIVE="${WWAN_PROXY_ARCHIVE:-}"
LOCAL_CHECKSUMS="${WWAN_PROXY_CHECKSUMS:-}"
HEALTH_URL="${WWAN_PROXY_HEALTH_URL:-http://127.0.0.1:9090/api/health}"
START_TIMEOUT="${WWAN_PROXY_START_TIMEOUT:-30}"
NO_START="${WWAN_PROXY_NO_START:-0}"
SKIP_HEALTH="${WWAN_PROXY_SKIP_HEALTH:-0}"
FORCE_OS="${WWAN_PROXY_FORCE_OS:-0}"

WORK_DIR=""
BACKUP_DIR=""
LOCK_HELD=0
MUTATION_STARTED=0
SUCCESS=0
PREVIOUS_ACTIVE=0
PREVIOUS_ENABLED=0
HAD_BINARY=0
HAD_UNIT=0
HAD_DATABASE=0
SYSTEMD_FUNCTIONAL=0
SERVICE_STOPPED_FOR_INSTALL=0
BINARY_PUBLISH_STAGE=""
UNIT_PUBLISH_STAGE=""

usage() {
	cat <<'EOF'
Usage:
  install-ubuntu.sh [options]

Install or upgrade wwan-proxy on Ubuntu with systemd. The installer downloads
the verified static musl release for amd64 or arm64, so it does not depend on
the host's glibc version.

Options:
  --version TAG          Release tag to install (default: latest)
  --repo OWNER/REPO      GitHub repository (default: iskycc/wwan-proxy)
  --archive FILE         Install a local release archive
  --checksum FILE        SHA256SUMS for --archive (required with --archive)
  --health-url URL       Health endpoint after startup
  --start-timeout SEC    Startup timeout, 5-300 seconds (default: 30)
  --skip-health-check    Start the service without probing an HTTP endpoint
  --no-start             Install files but leave the service disabled/stopped
  --force-os             Allow non-Ubuntu systems with systemd
  -h, --help             Show this help

Environment equivalents:
  WWAN_PROXY_VERSION, WWAN_PROXY_REPOSITORY, WWAN_PROXY_ARCHIVE,
  WWAN_PROXY_CHECKSUMS, WWAN_PROXY_HEALTH_URL, WWAN_PROXY_START_TIMEOUT,
  WWAN_PROXY_SKIP_HEALTH, WWAN_PROXY_NO_START, WWAN_PROXY_FORCE_OS

Examples:
  curl -fsSL https://raw.githubusercontent.com/iskycc/wwan-proxy/main/scripts/install-ubuntu.sh | sudo bash
  sudo bash install-ubuntu.sh --version build-0123456789ab
  sudo bash install-ubuntu.sh --health-url http://127.0.0.1:9191/api/health
  sudo bash install-ubuntu.sh --archive ./wwan-proxy-linux-amd64-musl.tar.gz --checksum ./SHA256SUMS

Persistent installer log:
  /var/log/wwan-proxy-install.log
EOF
}

timestamp() {
	date -u '+%Y-%m-%dT%H:%M:%SZ'
}

log() {
	local level=$1
	shift
	local line
	line="$(timestamp) [${level}] $*"
	printf '%s\n' "${line}"
	if [[ -e "${INSTALL_LOG}" ]]; then
		printf '%s\n' "${line}" >>"${INSTALL_LOG}" 2>/dev/null || true
	fi
}

fatal() {
	log ERROR "$*"
	exit 1
}

is_true() {
	case "$1" in
		1|true|TRUE|yes|YES|on|ON) return 0 ;;
		*) return 1 ;;
	esac
}

restore_path() {
	local had_path=$1
	local backup_path=$2
	local target_path=$3
	rm -f -- "${target_path}"
	if [[ "${had_path}" -eq 1 ]]; then
		cp -a -- "${backup_path}" "${target_path}"
	fi
}

rollback() {
	log WARN "installation failed after modifying service files; restoring the previous installation"
	set +e
	if [[ "${SYSTEMD_FUNCTIONAL}" -eq 1 ]]; then
		systemctl stop "${SERVICE_NAME}.service" >/dev/null 2>&1
	fi
	restore_path "${HAD_BINARY}" "${BACKUP_DIR}/wwan-proxy" "${INSTALL_BINARY}"
	restore_path "${HAD_UNIT}" "${BACKUP_DIR}/wwan-proxy.service" "${INSTALL_UNIT}"
	restore_path "${HAD_DATABASE}" "${BACKUP_DIR}/wwan-proxy.db" "${DATABASE_PATH}"
	rm -f -- "${DATABASE_PATH}-wal" "${DATABASE_PATH}-shm"
	if [[ "${SYSTEMD_FUNCTIONAL}" -eq 1 ]]; then
		systemctl daemon-reload >/dev/null 2>&1
		if [[ "${PREVIOUS_ENABLED}" -eq 1 ]]; then
			systemctl enable "${SERVICE_NAME}.service" >/dev/null 2>&1
		else
			systemctl disable "${SERVICE_NAME}.service" >/dev/null 2>&1
		fi
		if [[ "${PREVIOUS_ACTIVE}" -eq 1 && "${HAD_BINARY}" -eq 1 && "${HAD_UNIT}" -eq 1 ]]; then
			systemctl start "${SERVICE_NAME}.service" >/dev/null 2>&1
		fi
	fi
	set -e
}

cleanup() {
	local status=$?
	trap - EXIT
	if [[ "${status}" -ne 0 && "${MUTATION_STARTED}" -eq 1 && "${SUCCESS}" -ne 1 ]]; then
		rollback
	fi
	if [[ -n "${WORK_DIR}" && -d "${WORK_DIR}" ]]; then
		rm -rf -- "${WORK_DIR}"
	fi
	[[ -n "${BINARY_PUBLISH_STAGE}" ]] && rm -f -- "${BINARY_PUBLISH_STAGE}"
	[[ -n "${UNIT_PUBLISH_STAGE}" ]] && rm -f -- "${UNIT_PUBLISH_STAGE}"
	if [[ "${status}" -ne 0 && "${MUTATION_STARTED}" -ne 1 && "${SERVICE_STOPPED_FOR_INSTALL}" -eq 1 && "${PREVIOUS_ACTIVE}" -eq 1 ]]; then
		systemctl start "${SERVICE_NAME}.service" >/dev/null 2>&1 || true
	fi
	if [[ "${LOCK_HELD}" -eq 1 && -d "${LOCK_DIR}" ]]; then
		rm -f -- "${LOCK_DIR}/pid"
		rmdir -- "${LOCK_DIR}" 2>/dev/null || true
	fi
	exit "${status}"
}

while [[ "$#" -gt 0 ]]; do
	case "$1" in
		--version)
			[[ "$#" -ge 2 ]] || { echo "${PROGRAM_NAME}: --version requires a value" >&2; exit 2; }
			RELEASE_VERSION=$2
			shift 2
			;;
		--repo)
			[[ "$#" -ge 2 ]] || { echo "${PROGRAM_NAME}: --repo requires a value" >&2; exit 2; }
			REPOSITORY=$2
			shift 2
			;;
		--archive)
			[[ "$#" -ge 2 ]] || { echo "${PROGRAM_NAME}: --archive requires a value" >&2; exit 2; }
			LOCAL_ARCHIVE=$2
			shift 2
			;;
		--checksum|--checksums)
			[[ "$#" -ge 2 ]] || { echo "${PROGRAM_NAME}: --checksum requires a value" >&2; exit 2; }
			LOCAL_CHECKSUMS=$2
			shift 2
			;;
		--health-url)
			[[ "$#" -ge 2 ]] || { echo "${PROGRAM_NAME}: --health-url requires a value" >&2; exit 2; }
			HEALTH_URL=$2
			shift 2
			;;
		--start-timeout)
			[[ "$#" -ge 2 ]] || { echo "${PROGRAM_NAME}: --start-timeout requires a value" >&2; exit 2; }
			START_TIMEOUT=$2
			shift 2
			;;
		--skip-health-check)
			SKIP_HEALTH=1
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

[[ "$#" -eq 0 ]] || { echo "${PROGRAM_NAME}: unexpected positional arguments: $*" >&2; exit 2; }
[[ "$(id -u)" -eq 0 ]] || { echo "${PROGRAM_NAME}: root privileges are required" >&2; exit 1; }

is_true "${NO_START}" && NO_START=1 || NO_START=0
is_true "${SKIP_HEALTH}" && SKIP_HEALTH=1 || SKIP_HEALTH=0

mkdir -p /var/log /run
if [[ -L "${INSTALL_LOG}" || ( -e "${INSTALL_LOG}" && ! -f "${INSTALL_LOG}" ) ]]; then
	echo "${PROGRAM_NAME}: refusing unsafe installer log target: ${INSTALL_LOG}" >&2
	exit 1
fi
touch "${INSTALL_LOG}"
chmod 0600 "${INSTALL_LOG}"

trap cleanup EXIT
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM

case "${REPOSITORY}" in
	*/*)
		repository_owner=${REPOSITORY%%/*}
		repository_name=${REPOSITORY#*/}
		[[ "${repository_owner}" =~ ^[A-Za-z0-9_.-]+$ ]] || fatal "invalid GitHub repository owner: ${repository_owner}"
		[[ "${repository_name}" =~ ^[A-Za-z0-9_.-]+$ ]] || fatal "invalid GitHub repository name: ${repository_name}"
		;;
	*) fatal "GitHub repository must use OWNER/REPO form: ${REPOSITORY}" ;;
esac
[[ "${RELEASE_VERSION}" =~ ^[A-Za-z0-9._-]+$ ]] || fatal "invalid release tag: ${RELEASE_VERSION}"
[[ "${START_TIMEOUT}" =~ ^[0-9]+$ ]] || fatal "--start-timeout must be an integer"
(( START_TIMEOUT >= 5 && START_TIMEOUT <= 300 )) || fatal "--start-timeout must be between 5 and 300 seconds"
if [[ -n "${LOCAL_ARCHIVE}" && -z "${LOCAL_CHECKSUMS}" ]]; then
	fatal "--archive requires --checksum; unverified local packages are not installed"
fi
if [[ -z "${LOCAL_ARCHIVE}" && -n "${LOCAL_CHECKSUMS}" ]]; then
	fatal "--checksum is only valid together with --archive"
fi
case "${HEALTH_URL}" in
	http://*|https://*) ;;
	*) fatal "--health-url must start with http:// or https://" ;;
esac
case "${HEALTH_URL}" in
	*://*@*|*\?*|*\#*) fatal "health URL must not contain credentials, query parameters, or fragments" ;;
esac

if ! mkdir "${LOCK_DIR}" 2>/dev/null; then
	lock_pid=$(cat "${LOCK_DIR}/pid" 2>/dev/null || true)
	if [[ "${lock_pid}" =~ ^[0-9]+$ ]] && kill -0 "${lock_pid}" 2>/dev/null; then
		fatal "another Ubuntu installer is running (pid=${lock_pid})"
	fi
	rm -f -- "${LOCK_DIR}/pid"
	rmdir -- "${LOCK_DIR}" 2>/dev/null || fatal "stale installer lock contains unexpected files: ${LOCK_DIR}"
	mkdir "${LOCK_DIR}"
fi
LOCK_HELD=1
printf '%s\n' "$$" >"${LOCK_DIR}/pid"

WORK_DIR=$(mktemp -d /tmp/wwan-proxy-ubuntu-install.XXXXXX)
mkdir -p "${WORK_DIR}/extract"

if [[ ! -r /etc/os-release ]]; then
	is_true "${FORCE_OS}" || fatal "/etc/os-release is missing; this installer requires Ubuntu"
	OS_ID=unknown
	OS_VERSION=unknown
else
	# shellcheck disable=SC1091
	. /etc/os-release
	OS_ID=${ID:-unknown}
	OS_VERSION=${VERSION_ID:-unknown}
	if [[ "${OS_ID}" != ubuntu ]] && ! is_true "${FORCE_OS}"; then
		fatal "detected ${OS_ID} ${OS_VERSION}; this installer requires Ubuntu (use --force-os only after testing)"
	fi
fi

release_supported=0
for supported_release in ${SUPPORTED_UBUNTU_LTS}; do
	[[ "${OS_VERSION}" == "${supported_release}" ]] && release_supported=1
done
if [[ "${OS_ID}" == ubuntu && "${release_supported}" -ne 1 ]]; then
	log WARN "Ubuntu ${OS_VERSION} is not one of the tested LTS releases (${SUPPORTED_UBUNTU_LTS}); continuing because the release binary is static"
fi

command -v apt-get >/dev/null 2>&1 || fatal "apt-get is unavailable"
export DEBIAN_FRONTEND=noninteractive
missing_packages=()
dpkg-query -W -f='${Status}' ca-certificates 2>/dev/null | grep -Fq 'install ok installed' || missing_packages+=(ca-certificates)
command -v curl >/dev/null 2>&1 || missing_packages+=(curl)
command -v tar >/dev/null 2>&1 || missing_packages+=(tar)
if ! command -v systemctl >/dev/null 2>&1 || ! command -v systemd-analyze >/dev/null 2>&1; then
	missing_packages+=(systemd)
fi
if (( ${#missing_packages[@]} > 0 )); then
	log INFO "installing Ubuntu dependencies: ${missing_packages[*]}"
	apt-get update
	apt-get install -y --no-install-recommends "${missing_packages[@]}"
else
	log INFO "required Ubuntu commands are already installed; apt network access skipped"
fi
for required_command in curl tar sha256sum systemctl systemd-analyze useradd groupadd getent dpkg journalctl; do
	command -v "${required_command}" >/dev/null 2>&1 || fatal "required command is unavailable: ${required_command}"
done

if [[ -d /run/systemd/system ]] && systemctl show-environment >/dev/null 2>&1; then
	SYSTEMD_FUNCTIONAL=1
fi
if ! is_true "${NO_START}" && [[ "${SYSTEMD_FUNCTIONAL}" -ne 1 ]]; then
	fatal "systemd is not the active service manager; use --no-start only for image preparation"
fi

dpkg_arch=$(dpkg --print-architecture)
case "${dpkg_arch}" in
	amd64) PACKAGE_NAME="wwan-proxy-linux-amd64-musl" ;;
	arm64) PACKAGE_NAME="wwan-proxy-linux-arm64-musl" ;;
	*) fatal "unsupported Ubuntu architecture ${dpkg_arch}; supported architectures are amd64 and arm64" ;;
esac
ASSET_NAME="${PACKAGE_NAME}.tar.gz"
log INFO "Ubuntu=${OS_VERSION} architecture=${dpkg_arch} release=${RELEASE_VERSION} asset=${ASSET_NAME}"

download_file() {
	local description=$1
	local url=$2
	local target=$3
	log INFO "${description}"
	curl --fail-with-body --location --show-error --silent \
		--retry 3 --retry-all-errors --retry-delay 2 \
		--connect-timeout 15 --max-time 300 \
		--output "${target}" "${url}"
}

if [[ -n "${LOCAL_ARCHIVE}" ]]; then
	[[ -r "${LOCAL_ARCHIVE}" ]] || fatal "local archive is not readable: ${LOCAL_ARCHIVE}"
	[[ -r "${LOCAL_CHECKSUMS}" ]] || fatal "local checksum file is not readable: ${LOCAL_CHECKSUMS}"
	ARCHIVE_PATH=${LOCAL_ARCHIVE}
	CHECKSUMS_PATH=${LOCAL_CHECKSUMS}
	ASSET_NAME=$(basename "${LOCAL_ARCHIVE}")
	log INFO "using local archive=${ARCHIVE_PATH} checksum=${CHECKSUMS_PATH}"
else
	if [[ "${RELEASE_VERSION}" == latest ]]; then
		RELEASE_METADATA="${WORK_DIR}/release.json"
		download_file "resolving latest GitHub release" \
			"https://api.github.com/repos/${REPOSITORY}/releases/latest" "${RELEASE_METADATA}"
		RELEASE_VERSION=$(sed -n 's/^[[:space:]]*"tag_name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' "${RELEASE_METADATA}" | head -n 1)
		[[ "${RELEASE_VERSION}" =~ ^[A-Za-z0-9._-]+$ ]] || fatal "GitHub returned an invalid release tag"
	fi
	RELEASE_BASE="https://github.com/${REPOSITORY}/releases/download/${RELEASE_VERSION}"
	ARCHIVE_PATH="${WORK_DIR}/${ASSET_NAME}"
	CHECKSUMS_PATH="${WORK_DIR}/SHA256SUMS"
	download_file "downloading ${ASSET_NAME}" "${RELEASE_BASE}/${ASSET_NAME}" "${ARCHIVE_PATH}"
	download_file "downloading SHA256SUMS" "${RELEASE_BASE}/SHA256SUMS" "${CHECKSUMS_PATH}"
fi

mapfile -t checksum_matches < <(awk -v wanted="${ASSET_NAME}" '
{
	name=$2
	sub(/^\*/, "", name)
	sub(/^\.\//, "", name)
	if (name == wanted) print tolower($1)
}' "${CHECKSUMS_PATH}")
[[ "${#checksum_matches[@]}" -eq 1 ]] || fatal "SHA256SUMS must contain exactly one entry for ${ASSET_NAME}"
EXPECTED_SHA256=${checksum_matches[0]}
[[ "${EXPECTED_SHA256}" =~ ^[0-9a-f]{64}$ ]] || fatal "invalid SHA-256 for ${ASSET_NAME}"
ACTUAL_SHA256=$(sha256sum "${ARCHIVE_PATH}" | awk '{print tolower($1)}')
[[ "${ACTUAL_SHA256}" == "${EXPECTED_SHA256}" ]] || fatal "SHA-256 verification failed for ${ASSET_NAME}"
log INFO "verified ${ASSET_NAME} sha256=${ACTUAL_SHA256}"

ARCHIVE_LIST="${WORK_DIR}/archive-list"
tar -tzf "${ARCHIVE_PATH}" >"${ARCHIVE_LIST}"
if ! awk -F/ '
BEGIN { bad=0 }
substr($0, 1, 1) == "/" { bad=1 }
{ for (i=1; i<=NF; i++) if ($i == "..") bad=1 }
END { exit bad }
' "${ARCHIVE_LIST}"; then
	fatal "release archive contains an unsafe absolute or parent path"
fi
mapfile -t top_dirs < <(awk -F/ 'NF > 0 && $1 != "." && $1 != "" { print $1 }' "${ARCHIVE_LIST}" | sort -u)
[[ "${#top_dirs[@]}" -eq 1 ]] || fatal "release archive must contain exactly one top-level directory"
TOP_DIR=${top_dirs[0]}
[[ "${TOP_DIR}" =~ ^[A-Za-z0-9._-]+$ && "${TOP_DIR}" != "." && "${TOP_DIR}" != ".." ]] || fatal "unsafe archive directory: ${TOP_DIR}"
tar -xzf "${ARCHIVE_PATH}" -C "${WORK_DIR}/extract"
PACKAGE_ROOT="${WORK_DIR}/extract/${TOP_DIR}"
if find "${PACKAGE_ROOT}" -type l -print -quit | grep -q .; then
	fatal "release archive contains symbolic links"
fi
SOURCE_BINARY="${PACKAGE_ROOT}/wwan-proxy"
SOURCE_UNIT="${PACKAGE_ROOT}/wwan-proxy.service"
[[ -f "${SOURCE_BINARY}" && -x "${SOURCE_BINARY}" ]] || fatal "release package has no executable wwan-proxy binary"
[[ -f "${SOURCE_UNIT}" ]] || fatal "release package has no wwan-proxy.service"
"${SOURCE_BINARY}" -version >/dev/null || fatal "release binary cannot run on Ubuntu ${OS_VERSION}/${dpkg_arch}"
grep -Fqx 'User=wwan-proxy' "${SOURCE_UNIT}" || fatal "packaged unit has an unexpected service user"
grep -Fqx 'Group=wwan-proxy' "${SOURCE_UNIT}" || fatal "packaged unit has an unexpected service group"
grep -Fqx 'ExecStart=/usr/local/bin/wwan-proxy -db /var/lib/wwan-proxy/wwan-proxy.db -web-default 0.0.0.0:9090' "${SOURCE_UNIT}" || fatal "packaged unit has an unexpected ExecStart"
grep -Fqx 'AmbientCapabilities=CAP_NET_RAW' "${SOURCE_UNIT}" || fatal "packaged unit does not grant CAP_NET_RAW"
grep -Fqx 'LimitNOFILE=65536' "${SOURCE_UNIT}" || fatal "packaged unit does not set LimitNOFILE=65536"
grep -Fqx 'LimitNPROC=infinity' "${SOURCE_UNIT}" || fatal "packaged unit does not remove LimitNPROC"
grep -Fqx 'TasksMax=infinity' "${SOURCE_UNIT}" || fatal "packaged unit does not remove TasksMax"

if [[ "${SYSTEMD_FUNCTIONAL}" -eq 1 ]]; then
	systemctl is-active --quiet "${SERVICE_NAME}.service" && PREVIOUS_ACTIVE=1 || true
	systemctl is-enabled --quiet "${SERVICE_NAME}.service" && PREVIOUS_ENABLED=1 || true
fi
[[ -e "${INSTALL_BINARY}" ]] && HAD_BINARY=1
[[ -e "${INSTALL_UNIT}" ]] && HAD_UNIT=1
[[ -e "${DATABASE_PATH}" ]] && HAD_DATABASE=1

backup_stamp=$(date -u '+%Y%m%dT%H%M%SZ')
BACKUP_DIR="${BACKUP_ROOT}/${backup_stamp}-$$"
mkdir -p "${BACKUP_DIR}"
[[ "${HAD_BINARY}" -eq 1 ]] && cp -a -- "${INSTALL_BINARY}" "${BACKUP_DIR}/wwan-proxy"
[[ "${HAD_UNIT}" -eq 1 ]] && cp -a -- "${INSTALL_UNIT}" "${BACKUP_DIR}/wwan-proxy.service"

if ! getent group wwan-proxy >/dev/null; then
	groupadd --system wwan-proxy
fi
if ! getent passwd wwan-proxy >/dev/null; then
	useradd --system --gid wwan-proxy --home-dir "${DATA_DIR}" --no-create-home --shell /usr/sbin/nologin wwan-proxy
fi
mkdir -p "${DATA_DIR}"
chown wwan-proxy:wwan-proxy "${DATA_DIR}"
chmod 0750 "${DATA_DIR}"

if [[ "${SYSTEMD_FUNCTIONAL}" -eq 1 && "${PREVIOUS_ACTIVE}" -eq 1 ]]; then
	systemctl stop "${SERVICE_NAME}.service" || fatal "could not stop the existing service safely"
	SERVICE_STOPPED_FOR_INSTALL=1
fi
if [[ "${HAD_DATABASE}" -eq 1 ]]; then
	cp -a --reflink=auto "${DATABASE_PATH}" "${BACKUP_DIR}/wwan-proxy.db" || fatal "could not back up ${DATABASE_PATH}"
fi
MUTATION_STARTED=1

BINARY_PUBLISH_STAGE="/usr/local/bin/.wwan-proxy.install.$$"
UNIT_PUBLISH_STAGE="/etc/systemd/system/.wwan-proxy.service.install.$$"
install -m 0755 "${SOURCE_BINARY}" "${BINARY_PUBLISH_STAGE}"
install -m 0644 "${SOURCE_UNIT}" "${UNIT_PUBLISH_STAGE}"
mv -f -- "${BINARY_PUBLISH_STAGE}" "${INSTALL_BINARY}"
mv -f -- "${UNIT_PUBLISH_STAGE}" "${INSTALL_UNIT}"
BINARY_PUBLISH_STAGE=""
UNIT_PUBLISH_STAGE=""
chown root:root "${INSTALL_BINARY}" "${INSTALL_UNIT}"
"${INSTALL_BINARY}" -version >/dev/null
systemd-analyze verify "${INSTALL_UNIT}"

if is_true "${NO_START}"; then
	if [[ "${SYSTEMD_FUNCTIONAL}" -eq 1 ]]; then
		systemctl daemon-reload
		systemctl disable "${SERVICE_NAME}.service" >/dev/null 2>&1 || true
	fi
	SUCCESS=1
	log WARN "--no-start selected; service files were installed but the service is disabled and stopped"
	log INFO "enable later: systemctl enable --now ${SERVICE_NAME}.service"
else
	systemctl daemon-reload
	systemctl enable "${SERVICE_NAME}.service"
	systemctl start "${SERVICE_NAME}.service"

	deadline=$((SECONDS + START_TIMEOUT))
	while (( SECONDS < deadline )); do
		if ! systemctl is-active --quiet "${SERVICE_NAME}.service"; then
			sleep 1
			continue
		fi
		if is_true "${SKIP_HEALTH}"; then
			break
		fi
		if curl --fail --silent --show-error --connect-timeout 2 --max-time 3 "${HEALTH_URL}" >/dev/null 2>&1; then
			break
		fi
		sleep 1
	done
	systemctl is-active --quiet "${SERVICE_NAME}.service" || {
		journalctl -u "${SERVICE_NAME}.service" -n 80 --no-pager >>"${INSTALL_LOG}" 2>&1 || true
		fatal "systemd service did not remain active"
	}
	if ! is_true "${SKIP_HEALTH}"; then
		curl --fail --silent --show-error --connect-timeout 2 --max-time 3 "${HEALTH_URL}" >/dev/null 2>&1 || {
			journalctl -u "${SERVICE_NAME}.service" -n 80 --no-pager >>"${INSTALL_LOG}" 2>&1 || true
			fatal "health check failed after ${START_TIMEOUT}s: ${HEALTH_URL}"
		}
	fi
	SUCCESS=1
	log INFO "service is active and enabled; health_url=${HEALTH_URL} health_skipped=${SKIP_HEALTH}"
	if [[ "${HAD_DATABASE}" -eq 0 ]]; then
		log WARN "first-run WebUI listens on 0.0.0.0:9090; initialize the administrator immediately and restrict TCP/9090 to a trusted management network"
		log INFO "open WebUI at http://<ubuntu-host-ip>:9090 (the local health probe remains ${HEALTH_URL})"
	else
		log INFO "existing SQLite WebUI listener was preserved; 0.0.0.0:9090 is used only when no listener has been saved"
	fi
fi

installed_version=$("${INSTALL_BINARY}" -version 2>&1 | head -n 1)
log INFO "installed version=${installed_version} binary=${INSTALL_BINARY}"
log INFO "systemd limits: LimitNOFILE=65536 LimitNPROC=infinity TasksMax=infinity"
log INFO "database=${DATABASE_PATH} backup=${BACKUP_DIR}"
log INFO "installer log=${INSTALL_LOG}"

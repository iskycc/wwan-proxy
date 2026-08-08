#!/bin/sh

# Verified, repeatable installer/updater for OpenWrt. BusyBox ash compatible.

set -u

PROGRAM_NAME="install-openwrt.sh"
SERVICE_NAME="wwan-proxy"
UPDATER_SERVICE_NAME="wwan-proxy-updater"
DEFAULT_REPOSITORY="iskycc/wwan-proxy"

INSTALL_DIR="/opt/wwan-proxy"
INSTALL_BINARY="${INSTALL_DIR}/wwan-proxy"
INSTALL_INSTALLER="${INSTALL_DIR}/install-openwrt.sh"
INSTALL_INIT="/etc/init.d/wwan-proxy"
INSTALL_UPDATER_INIT="/etc/init.d/wwan-proxy-updater"
DATABASE_PATH="${INSTALL_DIR}/wwan-proxy.db"
BACKUP_ROOT="${INSTALL_DIR}/backups"
LOCK_DIR="/var/run/wwan-proxy-install.lock"

REPOSITORY="${WWAN_PROXY_REPOSITORY:-${DEFAULT_REPOSITORY}}"
RELEASE_VERSION="${WWAN_PROXY_VERSION:-latest}"
LOCAL_ARCHIVE="${WWAN_PROXY_ARCHIVE:-}"
LOCAL_CHECKSUMS="${WWAN_PROXY_CHECKSUMS:-}"
DOWNLOAD_INTERFACE="${WWAN_PROXY_DOWNLOAD_INTERFACE:-}"
NO_START="${WWAN_PROXY_NO_START:-0}"
FORCE_OS="${WWAN_PROXY_FORCE_OS:-0}"
UPDATE_AGENT_RUN="${WWAN_PROXY_UPDATE_AGENT:-0}"

WORK_DIR=""
LOCK_HELD=0
MUTATION_STARTED=0
SUCCESS=0
PREVIOUS_RUNNING=0
PREVIOUS_ENABLED=0
PREVIOUS_UPDATER_ENABLED=0
HAD_BINARY=0
HAD_INIT=0
HAD_UPDATER_INIT=0
HAD_INSTALLER=0
BINARY_STAGE=""
INIT_STAGE=""
UPDATER_INIT_STAGE=""
INSTALLER_STAGE=""

usage() {
	cat <<'EOF'
Usage:
  install-openwrt.sh [options]

Install or update wwan-proxy on OpenWrt using a SHA256-verified static musl
release. Running the installer again upgrades the program and preserves SQLite.

Options:
  --version TAG       Release tag to install (default: latest)
  --repo OWNER/REPO   GitHub repository (default: iskycc/wwan-proxy)
  --archive FILE      Install a local release archive
  --checksum FILE     SHA256SUMS for --archive (required with --archive)
  --download-interface IFACE
                      Bind GitHub metadata and package downloads to IFACE
  --no-start          Install files but leave both services disabled/stopped
  --force-os          Permit installation when OpenWrt cannot be detected
  -h, --help          Show this help

Examples:
  wget -qO- https://raw.githubusercontent.com/iskycc/wwan-proxy/main/scripts/install-openwrt.sh | sh
  sh install-openwrt.sh --version build-0123456789ab
  sh install-openwrt.sh --download-interface wwan0
EOF
}

log() {
	level=$1
	shift
	printf '%s [%s] %s\n' "$(date -u '+%Y-%m-%dT%H:%M:%SZ')" "${level}" "$*"
}

fatal() {
	log ERROR "$*" >&2
	exit 1
}

is_true() {
	case "$1" in
		1|true|TRUE|yes|YES|on|ON) return 0 ;;
		*) return 1 ;;
	esac
}

restore_file() {
	had_file=$1
	backup_file=$2
	target_file=$3
	rm -f -- "${target_file}"
	if [ "${had_file}" -eq 1 ]; then
		cp -p "${backup_file}" "${target_file}" || return 1
	fi
}

rollback() {
	log WARN "installation failed; restoring the previous executable and init scripts"
	"${INSTALL_INIT}" stop >/dev/null 2>&1 || true
	restore_failed=0
	restore_file "${HAD_BINARY}" "${WORK_DIR}/rollback/wwan-proxy" "${INSTALL_BINARY}" || restore_failed=1
	restore_file "${HAD_INIT}" "${WORK_DIR}/rollback/wwan-proxy.init" "${INSTALL_INIT}" || restore_failed=1
	restore_file "${HAD_UPDATER_INIT}" "${WORK_DIR}/rollback/wwan-proxy-updater.init" "${INSTALL_UPDATER_INIT}" || restore_failed=1
	restore_file "${HAD_INSTALLER}" "${WORK_DIR}/rollback/install-openwrt.sh" "${INSTALL_INSTALLER}" || restore_failed=1
	if [ "${PREVIOUS_ENABLED}" -eq 1 ] && [ -x "${INSTALL_INIT}" ]; then
		"${INSTALL_INIT}" enable >/dev/null 2>&1 || true
	elif [ -x "${INSTALL_INIT}" ]; then
		"${INSTALL_INIT}" disable >/dev/null 2>&1 || true
	fi
	if [ "${PREVIOUS_UPDATER_ENABLED}" -eq 1 ] && [ -x "${INSTALL_UPDATER_INIT}" ]; then
		"${INSTALL_UPDATER_INIT}" enable >/dev/null 2>&1 || true
	elif [ -x "${INSTALL_UPDATER_INIT}" ]; then
		"${INSTALL_UPDATER_INIT}" disable >/dev/null 2>&1 || true
	fi
	if [ "${PREVIOUS_RUNNING}" -eq 1 ] && [ "${HAD_BINARY}" -eq 1 ] && [ "${HAD_INIT}" -eq 1 ]; then
		"${INSTALL_INIT}" start >/dev/null 2>&1 || restore_failed=1
	fi
	if [ "${restore_failed}" -eq 0 ]; then
		log WARN "automatic rollback completed; SQLite data was not modified"
	else
		log ERROR "automatic rollback was incomplete; inspect ${INSTALL_DIR} and system logs"
	fi
}

cleanup() {
	status=$?
	trap - EXIT HUP INT TERM
	if [ "${status}" -ne 0 ] && [ "${MUTATION_STARTED}" -eq 1 ] && [ "${SUCCESS}" -ne 1 ]; then
		rollback
	fi
	case "${BINARY_STAGE}" in "${INSTALL_DIR}"/.wwan-proxy.install-*) rm -f -- "${BINARY_STAGE}" 2>/dev/null || true ;; esac
	case "${INIT_STAGE}" in /etc/init.d/.wwan-proxy.install-*) rm -f -- "${INIT_STAGE}" 2>/dev/null || true ;; esac
	case "${UPDATER_INIT_STAGE}" in /etc/init.d/.wwan-proxy-updater.install-*) rm -f -- "${UPDATER_INIT_STAGE}" 2>/dev/null || true ;; esac
	case "${INSTALLER_STAGE}" in "${INSTALL_DIR}"/.install-openwrt.install-*) rm -f -- "${INSTALLER_STAGE}" 2>/dev/null || true ;; esac
	[ -z "${WORK_DIR}" ] || rm -rf -- "${WORK_DIR}"
	if [ "${LOCK_HELD}" -eq 1 ]; then
		rm -f -- "${LOCK_DIR}/pid" 2>/dev/null || true
		rmdir -- "${LOCK_DIR}" 2>/dev/null || true
	fi
	exit "${status}"
}

trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM
trap 'cleanup' EXIT

while [ "$#" -gt 0 ]; do
	case "$1" in
		--version) [ "$#" -ge 2 ] || fatal "--version requires a value"; RELEASE_VERSION=$2; shift 2 ;;
		--repo) [ "$#" -ge 2 ] || fatal "--repo requires a value"; REPOSITORY=$2; shift 2 ;;
		--archive) [ "$#" -ge 2 ] || fatal "--archive requires a value"; LOCAL_ARCHIVE=$2; shift 2 ;;
		--checksum|--checksums) [ "$#" -ge 2 ] || fatal "--checksum requires a value"; LOCAL_CHECKSUMS=$2; shift 2 ;;
		--download-interface) [ "$#" -ge 2 ] || fatal "--download-interface requires a value"; DOWNLOAD_INTERFACE=$2; shift 2 ;;
		--no-start) NO_START=1; shift ;;
		--force-os) FORCE_OS=1; shift ;;
		-h|--help) usage; exit 0 ;;
		--) shift; break ;;
		*) fatal "unknown option: $1" ;;
	esac
done
[ "$#" -eq 0 ] || fatal "unexpected positional arguments: $*"
[ "$(id -u)" -eq 0 ] || fatal "root privileges are required"

case "${REPOSITORY}" in
	*/*) ;;
	*) fatal "GitHub repository must use OWNER/REPO form" ;;
esac
repository_owner=${REPOSITORY%%/*}
repository_name=${REPOSITORY#*/}
case "${repository_owner}" in *[!A-Za-z0-9_.-]*|"") fatal "invalid repository owner" ;; esac
case "${repository_name}" in *[!A-Za-z0-9_.-]*|""|*/*) fatal "invalid repository name" ;; esac
case "${RELEASE_VERSION}" in *[!A-Za-z0-9._-]*|"") fatal "invalid release tag" ;; esac
if [ -n "${LOCAL_ARCHIVE}" ] && [ -z "${LOCAL_CHECKSUMS}" ]; then fatal "--archive requires --checksum"; fi
if [ -z "${LOCAL_ARCHIVE}" ] && [ -n "${LOCAL_CHECKSUMS}" ]; then fatal "--checksum is only valid with --archive"; fi
if [ -n "${DOWNLOAD_INTERFACE}" ]; then
	case "${DOWNLOAD_INTERFACE}" in *[!A-Za-z0-9_.:-]*|"") fatal "invalid download interface: ${DOWNLOAD_INTERFACE}" ;; esac
	[ "${#DOWNLOAD_INTERFACE}" -le 15 ] || fatal "download interface exceeds 15 bytes: ${DOWNLOAD_INTERFACE}"
	[ -d "/sys/class/net/${DOWNLOAD_INTERFACE}" ] || fatal "download interface does not exist: ${DOWNLOAD_INTERFACE}"
fi

if [ ! -r /etc/openwrt_release ] && [ ! -r /etc/openwrt_version ] && ! is_true "${FORCE_OS}"; then
	fatal "OpenWrt was not detected; use --force-os only after testing"
fi
for required_command in tar gzip sha256sum awk sed sort find mktemp; do
	command -v "${required_command}" >/dev/null 2>&1 || fatal "required command is unavailable: ${required_command}"
done

case "$(uname -m)" in
	x86_64|amd64) PACKAGE_NAME="wwan-proxy-linux-amd64-musl" ;;
	aarch64|arm64) PACKAGE_NAME="wwan-proxy-linux-arm64-musl" ;;
	*) fatal "unsupported architecture $(uname -m); supported architectures are x86_64 and aarch64" ;;
esac
ASSET_NAME="${PACKAGE_NAME}.tar.gz"

mkdir -p /var/run || fatal "cannot create /var/run"
if ! mkdir "${LOCK_DIR}" 2>/dev/null; then
	lock_pid=$(cat "${LOCK_DIR}/pid" 2>/dev/null || true)
	case "${lock_pid}" in
		*[!0-9]*|"") ;;
		*) kill -0 "${lock_pid}" 2>/dev/null && fatal "another installer is running (pid=${lock_pid})" ;;
	esac
	rm -f -- "${LOCK_DIR}/pid" 2>/dev/null || true
	rmdir -- "${LOCK_DIR}" 2>/dev/null || fatal "stale installer lock contains unexpected files: ${LOCK_DIR}"
	mkdir "${LOCK_DIR}" || fatal "could not acquire installer lock"
fi
LOCK_HELD=1
printf '%s\n' "$$" >"${LOCK_DIR}/pid"

WORK_DIR=$(mktemp -d /tmp/wwan-proxy-install.XXXXXX) || fatal "cannot create temporary workspace"
mkdir -p "${WORK_DIR}/extract" "${WORK_DIR}/rollback" || fatal "cannot initialize temporary workspace"

download_file() {
	download_url=$1
	download_target=$2
	if [ -n "${DOWNLOAD_INTERFACE}" ]; then
		if command -v curl >/dev/null 2>&1; then
			curl -q -fL --retry 3 --connect-timeout 15 --max-time 300 --interface "${DOWNLOAD_INTERFACE}" -o "${download_target}" "${download_url}"
		elif [ -x "${INSTALL_BINARY}" ]; then
			"${INSTALL_BINARY}" \
				-update-download-url "${download_url}" \
				-update-download-output "${download_target}" \
				-update-download-interface "${DOWNLOAD_INTERFACE}"
		else
			fatal "curl or an existing wwan-proxy installation is required to bind downloads to ${DOWNLOAD_INTERFACE}"
		fi
	elif command -v curl >/dev/null 2>&1; then
		curl -q -fL --retry 3 --connect-timeout 15 --max-time 300 -o "${download_target}" "${download_url}"
	elif command -v uclient-fetch >/dev/null 2>&1; then
		uclient-fetch -T 300 -O "${download_target}" "${download_url}"
	elif command -v wget >/dev/null 2>&1; then
		wget -T 300 -O "${download_target}" "${download_url}"
	else
		fatal "curl, uclient-fetch or wget is required"
	fi
}

if [ -n "${LOCAL_ARCHIVE}" ]; then
	[ -r "${LOCAL_ARCHIVE}" ] || fatal "local archive is not readable: ${LOCAL_ARCHIVE}"
	[ -r "${LOCAL_CHECKSUMS}" ] || fatal "local checksum file is not readable: ${LOCAL_CHECKSUMS}"
	ARCHIVE_PATH=${LOCAL_ARCHIVE}
	CHECKSUMS_PATH=${LOCAL_CHECKSUMS}
	ASSET_NAME=$(basename "${LOCAL_ARCHIVE}")
else
	if [ "${RELEASE_VERSION}" = latest ]; then
		RELEASE_METADATA="${WORK_DIR}/release.json"
		download_file "https://api.github.com/repos/${REPOSITORY}/releases/latest" "${RELEASE_METADATA}" || fatal "could not resolve latest release"
		RELEASE_VERSION=$(sed -n 's/^[[:space:]]*"tag_name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' "${RELEASE_METADATA}" | head -n 1)
		case "${RELEASE_VERSION}" in *[!A-Za-z0-9._-]*|"") fatal "GitHub returned an invalid release tag" ;; esac
	fi
	RELEASE_BASE="https://github.com/${REPOSITORY}/releases/download/${RELEASE_VERSION}"
	ARCHIVE_PATH="${WORK_DIR}/${ASSET_NAME}"
	CHECKSUMS_PATH="${WORK_DIR}/SHA256SUMS"
	download_file "${RELEASE_BASE}/${ASSET_NAME}" "${ARCHIVE_PATH}" || fatal "could not download ${ASSET_NAME}"
	download_file "${RELEASE_BASE}/SHA256SUMS" "${CHECKSUMS_PATH}" || fatal "could not download SHA256SUMS"
fi

CHECKSUM_MATCHES="${WORK_DIR}/checksum-matches"
awk -v wanted="${ASSET_NAME}" '{ name=$2; sub(/^\*/, "", name); sub(/^\.\//, "", name); if (name == wanted) print tolower($1) }' "${CHECKSUMS_PATH}" >"${CHECKSUM_MATCHES}" || fatal "could not parse SHA256SUMS"
[ "$(wc -l <"${CHECKSUM_MATCHES}" | tr -d '[:space:]')" -eq 1 ] || fatal "SHA256SUMS must contain exactly one entry for ${ASSET_NAME}"
EXPECTED_SHA256=$(head -n 1 "${CHECKSUM_MATCHES}")
case "${EXPECTED_SHA256}" in *[!0-9a-f]*|"") fatal "invalid SHA-256 for ${ASSET_NAME}" ;; esac
[ "${#EXPECTED_SHA256}" -eq 64 ] || fatal "invalid SHA-256 length for ${ASSET_NAME}"
ACTUAL_SHA256=$(sha256sum "${ARCHIVE_PATH}" | awk '{print tolower($1)}')
[ "${ACTUAL_SHA256}" = "${EXPECTED_SHA256}" ] || fatal "SHA-256 verification failed for ${ASSET_NAME}"
log INFO "verified ${ASSET_NAME} sha256=${ACTUAL_SHA256}"

ARCHIVE_LIST="${WORK_DIR}/archive-list"
tar -tzf "${ARCHIVE_PATH}" >"${ARCHIVE_LIST}" || fatal "release archive is unreadable"
awk -F/ 'BEGIN { bad=0 } substr($0,1,1)=="/" { bad=1 } { for(i=1;i<=NF;i++) if($i=="..") bad=1 } END { exit bad }' "${ARCHIVE_LIST}" || fatal "release archive contains unsafe paths"
TOP_DIRS="${WORK_DIR}/top-dirs"
awk -F/ 'NF>0 && $1!="." && $1!="" {print $1}' "${ARCHIVE_LIST}" | sort -u >"${TOP_DIRS}"
[ "$(wc -l <"${TOP_DIRS}" | tr -d '[:space:]')" -eq 1 ] || fatal "release archive must contain one top-level directory"
TOP_DIR=$(head -n 1 "${TOP_DIRS}")
case "${TOP_DIR}" in *[!A-Za-z0-9._-]*|""|.|..) fatal "unsafe archive directory" ;; esac
tar -xzf "${ARCHIVE_PATH}" -C "${WORK_DIR}/extract" || fatal "release extraction failed"
PACKAGE_ROOT="${WORK_DIR}/extract/${TOP_DIR}"
[ -z "$(find "${PACKAGE_ROOT}" -type l -print 2>/dev/null)" ] || fatal "release archive contains symbolic links"

SOURCE_BINARY="${PACKAGE_ROOT}/wwan-proxy"
SOURCE_INIT="${PACKAGE_ROOT}/wwan-proxy.init"
SOURCE_UPDATER_INIT="${PACKAGE_ROOT}/wwan-proxy-updater.init"
SOURCE_INSTALLER="${PACKAGE_ROOT}/install-openwrt.sh"
for required_file in "${SOURCE_BINARY}" "${SOURCE_INIT}" "${SOURCE_UPDATER_INIT}" "${SOURCE_INSTALLER}"; do
	[ -f "${required_file}" ] || fatal "release package is incomplete; missing ${required_file##*/}"
done
[ -x "${SOURCE_BINARY}" ] || fatal "release binary is not executable"
"${SOURCE_BINARY}" -version >/dev/null 2>&1 || fatal "release binary cannot run on this OpenWrt system"
sh -n "${SOURCE_INIT}" || fatal "main init script has invalid syntax"
sh -n "${SOURCE_UPDATER_INIT}" || fatal "updater init script has invalid syntax"
sh -n "${SOURCE_INSTALLER}" || fatal "persistent installer has invalid syntax"
grep -Fq 'PROG="/opt/wwan-proxy/wwan-proxy"' "${SOURCE_INIT}" || fatal "main init script has an unexpected binary path"
grep -Fq -- '-update-agent' "${SOURCE_UPDATER_INIT}" || fatal "updater init script has an unexpected command"

for protected_target in "${INSTALL_DIR}" "${INSTALL_BINARY}" "${INSTALL_INSTALLER}" "${INSTALL_INIT}" "${INSTALL_UPDATER_INIT}" "${DATABASE_PATH}" "${BACKUP_ROOT}"; do
	[ ! -L "${protected_target}" ] || fatal "refusing symbolic-link target: ${protected_target}"
done

[ -x "${INSTALL_INIT}" ] && "${INSTALL_INIT}" status >/dev/null 2>&1 && PREVIOUS_RUNNING=1
[ -x "${INSTALL_INIT}" ] && "${INSTALL_INIT}" enabled >/dev/null 2>&1 && PREVIOUS_ENABLED=1
[ -x "${INSTALL_UPDATER_INIT}" ] && "${INSTALL_UPDATER_INIT}" enabled >/dev/null 2>&1 && PREVIOUS_UPDATER_ENABLED=1
[ -f "${INSTALL_BINARY}" ] && HAD_BINARY=1
[ -f "${INSTALL_INIT}" ] && HAD_INIT=1
[ -f "${INSTALL_UPDATER_INIT}" ] && HAD_UPDATER_INIT=1
[ -f "${INSTALL_INSTALLER}" ] && HAD_INSTALLER=1

[ "${HAD_BINARY}" -eq 0 ] || cp -p "${INSTALL_BINARY}" "${WORK_DIR}/rollback/wwan-proxy" || fatal "could not back up installed binary"
[ "${HAD_INIT}" -eq 0 ] || cp -p "${INSTALL_INIT}" "${WORK_DIR}/rollback/wwan-proxy.init" || fatal "could not back up main init script"
[ "${HAD_UPDATER_INIT}" -eq 0 ] || cp -p "${INSTALL_UPDATER_INIT}" "${WORK_DIR}/rollback/wwan-proxy-updater.init" || fatal "could not back up updater init script"
[ "${HAD_INSTALLER}" -eq 0 ] || cp -p "${INSTALL_INSTALLER}" "${WORK_DIR}/rollback/install-openwrt.sh" || fatal "could not back up persistent installer"

mkdir -p "${INSTALL_DIR}" "${BACKUP_ROOT}" || fatal "could not create installation directories"
chmod 0755 "${INSTALL_DIR}" || fatal "could not secure installation directory"
chmod 0700 "${BACKUP_ROOT}" || fatal "could not secure backup directory"
if [ -f "${DATABASE_PATH}" ]; then
	backup_stamp=$(date -u '+%Y%m%dT%H%M%SZ')
	database_backup="${BACKUP_ROOT}/wwan-proxy.db.${backup_stamp}.$$"
	cp -p "${DATABASE_PATH}" "${database_backup}" || fatal "could not create SQLite safety backup"
	log INFO "SQLite safety backup=${database_backup}"
elif [ -e "${DATABASE_PATH}" ]; then
	fatal "database path is not a regular file: ${DATABASE_PATH}"
fi

if [ "${PREVIOUS_RUNNING}" -eq 1 ]; then
	"${INSTALL_INIT}" stop || fatal "could not stop the existing service safely"
fi
MUTATION_STARTED=1

BINARY_STAGE="${INSTALL_DIR}/.wwan-proxy.install-$$"
INIT_STAGE="/etc/init.d/.wwan-proxy.install-$$"
UPDATER_INIT_STAGE="/etc/init.d/.wwan-proxy-updater.install-$$"
INSTALLER_STAGE="${INSTALL_DIR}/.install-openwrt.install-$$"
cp "${SOURCE_BINARY}" "${BINARY_STAGE}" || fatal "could not stage binary"
cp "${SOURCE_INIT}" "${INIT_STAGE}" || fatal "could not stage main init script"
cp "${SOURCE_UPDATER_INIT}" "${UPDATER_INIT_STAGE}" || fatal "could not stage updater init script"
cp "${SOURCE_INSTALLER}" "${INSTALLER_STAGE}" || fatal "could not stage persistent installer"
chmod 0755 "${BINARY_STAGE}" "${INIT_STAGE}" "${UPDATER_INIT_STAGE}" "${INSTALLER_STAGE}" || fatal "could not set staged file permissions"
mv -f "${BINARY_STAGE}" "${INSTALL_BINARY}" || fatal "could not publish binary"
mv -f "${INIT_STAGE}" "${INSTALL_INIT}" || fatal "could not publish main init script"
mv -f "${UPDATER_INIT_STAGE}" "${INSTALL_UPDATER_INIT}" || fatal "could not publish updater init script"
mv -f "${INSTALLER_STAGE}" "${INSTALL_INSTALLER}" || fatal "could not publish persistent installer"
BINARY_STAGE=""
INIT_STAGE=""
UPDATER_INIT_STAGE=""
INSTALLER_STAGE=""
"${INSTALL_BINARY}" -version >/dev/null 2>&1 || fatal "installed binary preflight failed"

if is_true "${NO_START}"; then
	"${INSTALL_INIT}" stop >/dev/null 2>&1 || true
	"${INSTALL_UPDATER_INIT}" stop >/dev/null 2>&1 || true
	"${INSTALL_INIT}" disable >/dev/null 2>&1 || true
	"${INSTALL_UPDATER_INIT}" disable >/dev/null 2>&1 || true
	SUCCESS=1
	log WARN "--no-start selected; both services are installed but disabled and stopped"
	log INFO "enable later: ${INSTALL_INIT} enable; ${INSTALL_UPDATER_INIT} enable; ${INSTALL_INIT} start; ${INSTALL_UPDATER_INIT} start"
	exit 0
fi

"${INSTALL_INIT}" enable || fatal "could not enable main service"
"${INSTALL_UPDATER_INIT}" enable || fatal "could not enable updater service"
"${INSTALL_INIT}" start || fatal "could not start main service"
stability_count=0
while [ "${stability_count}" -lt 5 ]; do
	sleep 1
	"${INSTALL_INIT}" status >/dev/null 2>&1 || fatal "main service did not remain running"
	stability_count=$((stability_count + 1))
done
if ! is_true "${UPDATE_AGENT_RUN}"; then
	"${INSTALL_UPDATER_INIT}" restart >/dev/null 2>&1 || "${INSTALL_UPDATER_INIT}" start || fatal "could not start updater service"
fi

SUCCESS=1
installed_version=$("${INSTALL_BINARY}" -version 2>&1 | head -n 1)
log INFO "installed version=${installed_version} binary=${INSTALL_BINARY}"
log INFO "database preserved at ${DATABASE_PATH}; Web automatic updater=${INSTALL_UPDATER_INIT}"

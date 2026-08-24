#!/bin/sh
set -eu

readonly REPO="${VPS_TOOL_REPO:-wangxiangg1/vps-tool}"
readonly VERSION="${VPS_TOOL_VERSION:-latest}"
readonly INSTALL_ROOT="/etc/vps-agent"
readonly STATE_ROOT="/var/lib/vps-agent"
readonly BIN_PATH="/usr/local/bin/vps-agent"
readonly HELPER_PATH="/usr/local/libexec/vps-agent-helper"
readonly SERVICE_PATH="/etc/systemd/system/vps-agent.service"
readonly OPENRC_SERVICE_PATH="/etc/init.d/vps-agent"
readonly SUDOERS_PATH="/etc/sudoers.d/vps-agent-helper"
readonly DOAS_POLICY_PATH="/etc/doas.d/vps-agent.conf"
readonly USER_NAME="vps-agent"
readonly GROUP_NAME="vps-agent"
RESOLVED_VERSION=""
INIT_SYSTEM=""
PRIVILEGE_TOOL=""

log() { printf '[vps-tool] %s\n' "$*"; }
die() { printf '[vps-tool] error: %s\n' "$*" >&2; exit 1; }

cleanup() {
  if [ -n "${TMP_DIR:-}" ] && [ -d "$TMP_DIR" ]; then
    rm -rf -- "$TMP_DIR"
  fi
}
trap cleanup 0

require_root() {
  [ "$(id -u)" -eq 0 ] || die "run this installer as root"
}

require_command() {
  command -v "$1" >/dev/null 2>&1 || die "required command is missing: $1"
}

detect_init_system() {
  if [ -d /run/systemd/system ] && command -v systemctl >/dev/null 2>&1; then
    INIT_SYSTEM="systemd"
    return
  fi
  if command -v rc-service >/dev/null 2>&1 && [ -d /etc/init.d ]; then
    INIT_SYSTEM="openrc"
    return
  fi
  die "systemd or OpenRC is required for vps-agent"
}

ensure_alpine_dependencies() {
  if [ "$INIT_SYSTEM" != "openrc" ] || [ ! -f /etc/alpine-release ]; then
    return
  fi
  require_command apk
  missing=""
  command -v curl >/dev/null 2>&1 || missing="$missing curl"
  if { ! command -v sudo >/dev/null 2>&1 || ! command -v visudo >/dev/null 2>&1; } &&
    ! command -v doas >/dev/null 2>&1; then
    missing="$missing doas"
  fi
  command -v install >/dev/null 2>&1 || missing="$missing coreutils"
  [ -f /etc/ssl/certs/ca-certificates.crt ] || missing="$missing ca-certificates"
  if [ -n "$missing" ]; then
    # Alpine's base image commonly lacks curl, privilege escalation, and a CA bundle.
    # Install only the small packages needed by the installer and WSS client.
    apk add --no-cache $missing
  fi
}

select_privilege_tool() {
  if command -v sudo >/dev/null 2>&1 && command -v visudo >/dev/null 2>&1; then
    PRIVILEGE_TOOL="sudo"
    return
  fi
  if [ "$INIT_SYSTEM" = "openrc" ] && command -v doas >/dev/null 2>&1; then
    PRIVILEGE_TOOL="doas"
    return
  fi
  die "sudo with visudo, or doas on Alpine/OpenRC, is required"
}

detect_arch() {
  case "$(uname -m)" in
    x86_64|amd64) printf 'amd64\n' ;;
    aarch64|arm64) printf 'arm64\n' ;;
    *) die "unsupported architecture: $(uname -m); supported architectures are amd64 and arm64" ;;
  esac
}

resolve_version() {
  if [ "$VERSION" != "latest" ]; then
    printf '%s\n' "$VERSION"
    return
  fi
  local latest_url
  latest_url=$(curl --fail --silent --show-error --location --max-time 20 \
    -o /dev/null -w '%{url_effective}' "https://github.com/${REPO}/releases/latest")
  latest_url="${latest_url%/}"
  printf '%s\n' "${latest_url##*/}"
}

download_asset() {
  local version="$1"
  local asset="$2"
  local destination="$3"
  local url="https://github.com/${REPO}/releases/download/${version}/${asset}"
  curl --fail --silent --show-error --location --retry 3 --max-time 120 "$url" -o "$destination"
}

ensure_identity() {
  if command -v getent >/dev/null 2>&1; then
    group_exists="getent group $GROUP_NAME"
  else
    group_exists="grep -q ^$GROUP_NAME: /etc/group"
  fi
  if ! sh -c "$group_exists" >/dev/null 2>&1; then
    if [ "$INIT_SYSTEM" = "openrc" ]; then
      addgroup -S "$GROUP_NAME"
    else
      groupadd --system "$GROUP_NAME"
    fi
  fi
  if ! id -u "$USER_NAME" >/dev/null 2>&1; then
    if [ "$INIT_SYSTEM" = "openrc" ]; then
      adduser -S -D -H -h "$STATE_ROOT" -s /sbin/nologin -G "$GROUP_NAME" "$USER_NAME"
    else
      useradd --system --gid "$GROUP_NAME" --home-dir "$STATE_ROOT" --shell /usr/sbin/nologin "$USER_NAME"
    fi
  fi
}

validate_config() {
  local config_path="$INSTALL_ROOT/agent.json"
  if [ ! -f "$config_path" ]; then
    [ -n "${VPS_AGENT_NODE_ID:-}" ] || die "missing config; set VPS_AGENT_NODE_ID, VPS_AGENT_REGISTRATION_TOKEN, VPS_AGENT_WSS_URL and VPS_AGENT_XUI_UNIT"
    [ -n "${VPS_AGENT_REGISTRATION_TOKEN:-}" ] || [ -n "${VPS_AGENT_CREDENTIAL:-}" ] || die "set VPS_AGENT_REGISTRATION_TOKEN for first enrollment or VPS_AGENT_CREDENTIAL for an existing node"
    [ -n "${VPS_AGENT_WSS_URL:-}" ] || die "set VPS_AGENT_WSS_URL to a wss:// endpoint"
    case "$VPS_AGENT_WSS_URL" in wss://*) ;; *) die "VPS_AGENT_WSS_URL must use wss://" ;; esac
    [ -n "${VPS_AGENT_XUI_UNIT:-}" ] || die "set VPS_AGENT_XUI_UNIT"
    local adapter="${VPS_AGENT_WARP_ADAPTER:-generic}"
    case "$adapter" in generic|fixed-helper|warp-cli|wgcf|warp-go) ;; *) die "unsupported VPS_AGENT_WARP_ADAPTER: $adapter" ;; esac
    write_initial_config "$config_path" "$adapter"
  fi
  chown "$USER_NAME":"$GROUP_NAME" "$config_path"
  chmod 0600 "$config_path"
}

write_initial_config() {
  local config_path="$1"
  local adapter="$2"
  local escaped_node escaped_token escaped_credential escaped_wss escaped_unit
  escaped_node=$(json_escape "${VPS_AGENT_NODE_ID}")
  escaped_token=$(json_escape "${VPS_AGENT_REGISTRATION_TOKEN:-}")
  escaped_credential=$(json_escape "${VPS_AGENT_CREDENTIAL:-}")
  escaped_wss=$(json_escape "${VPS_AGENT_WSS_URL}")
  escaped_unit=$(json_escape "${VPS_AGENT_XUI_UNIT}")
  local escaped_version
  escaped_version=$(json_escape "${RESOLVED_VERSION#v}")
  cat >"$config_path" <<EOF
{
  "node_id": "$escaped_node",
  "credential": "$escaped_credential",
  "registration_token": "$escaped_token",
  "wss_url": "$escaped_wss",
  "warp_adapter": "$adapter",
  "xui_unit": "$escaped_unit",
  "helper_path": "$HELPER_PATH",
  "state_path": "$STATE_ROOT/requests.json",
  "agent_version": "$escaped_version",
  "dry_run": false
}
EOF
}

json_escape() {
  local value="$1"
  local newline carriage tab
  newline=$(printf '\nx')
  newline=${newline%x}
  carriage=$(printf '\rx')
  carriage=${carriage%x}
  tab=$(printf '\tx')
  tab=${tab%x}
  case "$value" in
    *"$newline"*|*"$carriage"*|*"$tab"*) die "configuration values must not contain newline, carriage return, or tab" ;;
  esac
  printf '%s' "$value" | sed 's/\\/\\\\/g; s/"/\\"/g'
}

install_files() {
  local arch="$1"
  install -d -o root -g root -m 0755 /usr/local/libexec
  install -o root -g root -m 0755 "$TMP_DIR/vps-agent-linux-${arch}" "$BIN_PATH"
  install -o root -g root -m 0755 "$TMP_DIR/vps-agent-helper-linux-${arch}" "$HELPER_PATH"
}

install_policy() {
  if [ "$PRIVILEGE_TOOL" = "doas" ]; then
    local policy_path="$TMP_DIR/vps-agent-helper.doas"
    cat >"$policy_path" <<EOF
# Managed by vps-tool installer. The agent can invoke only this fixed helper.
permit nopass ${USER_NAME} as root cmd ${HELPER_PATH}
EOF
    chmod 0600 "$policy_path"
    doas -C "$policy_path" >/dev/null || die "generated doas policy failed validation"
    install -d -o root -g root -m 0755 /etc/doas.d
    install -o root -g root -m 0600 "$policy_path" "$DOAS_POLICY_PATH"
    return
  fi
  local policy_path="$TMP_DIR/vps-agent-helper.sudoers"
  cat >"$policy_path" <<EOF
# Managed by vps-tool installer. The agent can invoke only this fixed helper.
${USER_NAME} ALL=(root) NOPASSWD: ${HELPER_PATH} *
EOF
  chmod 0440 "$policy_path"
  visudo -cf "$policy_path" >/dev/null || die "generated sudoers policy failed validation"
  install -o root -g root -m 0440 "$policy_path" "$SUDOERS_PATH"
}

install_service() {
  if [ "$INIT_SYSTEM" = "openrc" ]; then
    cat >"$OPENRC_SERVICE_PATH" <<EOF
#!/sbin/openrc-run

name="vps-tool low-resource agent"
command="${BIN_PATH}"
command_args="-config ${INSTALL_ROOT}/agent.json"
command_user="${USER_NAME}:${GROUP_NAME}"
supervisor="supervise-daemon"
respawn_delay=5
respawn_max=0

depend() {
  need net
  after firewall
}
EOF
    chmod 0755 "$OPENRC_SERVICE_PATH"
    chown root:root "$OPENRC_SERVICE_PATH"
    return
  fi
  cat >"$SERVICE_PATH" <<EOF
[Unit]
Description=vps-tool low-resource agent
After=network-online.target
Wants=network-online.target

[Service]
User=${USER_NAME}
Group=${GROUP_NAME}
ExecStart=${BIN_PATH} -config ${INSTALL_ROOT}/agent.json
Restart=always
RestartSec=5
PrivateTmp=true
ProtectSystem=strict
ProtectHome=true
ReadWritePaths=${INSTALL_ROOT} ${STATE_ROOT}
NoNewPrivileges=false
PrivateDevices=false
RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6
RestrictSUIDSGID=true
LockPersonality=true
MemoryDenyWriteExecute=true

[Install]
WantedBy=multi-user.target
EOF
  chmod 0644 "$SERVICE_PATH"
  chown root:root "$SERVICE_PATH"
}

main() {
  require_root
  detect_init_system
  ensure_alpine_dependencies
  select_privilege_tool
  require_command curl
  require_command install
  require_command sha256sum
  if [ "$PRIVILEGE_TOOL" = "doas" ]; then
    require_command doas
  else
    require_command sudo
    require_command visudo
  fi
  if [ "$INIT_SYSTEM" = "openrc" ]; then
    require_command addgroup
    require_command adduser
    require_command rc-service
    require_command rc-update
  else
    require_command systemctl
    require_command groupadd
    require_command useradd
  fi
  local arch version
  arch=$(detect_arch)
  version=$(resolve_version)
  case "$version" in v[0-9]*) ;; *) die "could not resolve a release tag; set VPS_TOOL_VERSION explicitly" ;; esac
  RESOLVED_VERSION="$version"
  TMP_DIR=$(mktemp -d)
  log "installing ${REPO} ${version} for linux/${arch}"
  download_asset "$version" "vps-agent-linux-${arch}" "$TMP_DIR/vps-agent-linux-${arch}"
  download_asset "$version" "vps-agent-helper-linux-${arch}" "$TMP_DIR/vps-agent-helper-linux-${arch}"
  download_asset "$version" "SHA256SUMS" "$TMP_DIR/SHA256SUMS"
  grep -E "  (vps-agent-linux-${arch}|vps-agent-helper-linux-${arch})$" "$TMP_DIR/SHA256SUMS" >"$TMP_DIR/CHECKSUMS"
  [ "$(wc -l <"$TMP_DIR/CHECKSUMS" | tr -d ' ')" -eq 2 ] || die "release checksum list is missing Agent assets"
  (cd "$TMP_DIR" && sha256sum -c CHECKSUMS) || die "release checksum verification failed"
  chmod 0755 "$TMP_DIR/vps-agent-linux-${arch}" "$TMP_DIR/vps-agent-helper-linux-${arch}"
  ensure_identity
  install -d -o "$USER_NAME" -g "$GROUP_NAME" -m 0700 "$INSTALL_ROOT"
  install -d -o "$USER_NAME" -g "$GROUP_NAME" -m 0700 "$STATE_ROOT"
  validate_config
  if [ "$INIT_SYSTEM" = "openrc" ] && [ ! -c /dev/net/tun ]; then
    log "warning: /dev/net/tun is unavailable; Agent enrollment can work, but WARP requires LXC TUN access and CAP_NET_ADMIN"
  fi
  install_files "$arch"
  install_policy
  install_service
  if [ "$INIT_SYSTEM" = "openrc" ]; then
    rc-update add vps-agent default >/dev/null
    if rc-service vps-agent status >/dev/null 2>&1; then
      rc-service vps-agent restart
    else
      rc-service vps-agent start
    fi
    rc-service vps-agent status >/dev/null 2>&1 || die "vps-agent OpenRC service did not become active"
    log "installed and started vps-agent through OpenRC"
  else
    systemctl daemon-reload
    systemctl enable --now vps-agent.service
    systemctl is-active --quiet vps-agent.service || die "vps-agent.service did not become active"
    log "installed and started vps-agent.service"
  fi
}

if [ "${VPS_TOOL_INSTALLER_LIBRARY_ONLY:-0}" != "1" ]; then
  main "$@"
fi

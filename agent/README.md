# vps-agent

This directory contains the low-resource Go Agent for vps-tool. It uses the
standard library only. The agent actively connects to the configured `wss://`
endpoint, sends a protocol envelope for every message, reports an initial full
status and then heartbeats every 30 seconds, and reconnects with bounded
exponential backoff (`1/2/4/8/16/30/60` seconds plus jitter).

## Build and test

```text
cd agent
gofmt -w cmd/vps-agent internal
go test ./...
go vet ./...
go build -trimpath -ldflags "-s -w" -o vps-agent ./cmd/vps-agent
```

Use `-once` to collect one local status report and exit. `-dry-run` is only a
local testing convenience; it does not bypass JSON, action, unit, or path
validation.

## Configuration

The file is read from `VPS_AGENT_CONFIG` or `-config`. It must be a regular
local file with no group/world permissions on Linux and is limited to 64 KiB.
Unknown JSON fields and trailing JSON are rejected. Explicit environment
variables with the `VPS_AGENT_` prefix can override individual fields:
`NODE_ID`, `CREDENTIAL`, `WSS_URL`, `WARP_ADAPTER`, `XUI_UNIT`, `HELPER_PATH`,
`STATE_PATH`, `VERSION`, and `DRY_RUN`.

```json
{
  "node_id": "node-01",
  "credential": "replace-with-a-long-random-secret",
  "wss_url": "wss://panel.example.com/agent",
  "warp_adapter": "generic",
  "xui_unit": "x-ui.service",
  "helper_path": "/usr/local/libexec/vps-agent-helper",
  "state_path": "/var/lib/vps-agent/requests.json",
  "agent_version": "0.1.0",
  "dry_run": false
}
```

For first-time enrollment, replace `credential` with the one-time
`registration_token` returned by the control plane. Enrollment requires
`-config`; after a successful handshake the Agent atomically replaces the token
with the issued long-term credential in the same `0600` file. Registered Agents
send the credential in an HTTPS/WSS `Authorization` header. It is never
appended to the URL, query string, or command arguments.

For local tests, use `dry_run: true` with the `dry-run` adapter. The fake
backend starts with WARP ON and cycles through two documentation IPs.

## One-line installation

The public release includes `vps-agent`, the fixed root Helper, an installer,
and `SHA256SUMS` for both Linux architectures. Create a node in the control
plane first. The control plane's one-time-token dialog generates one short
command that streams the installer from GitHub and passes the node settings and
one-time token as arguments. Run that command in a root shell on the target VPS;
the installer remains hosted in the GitHub release instead of being embedded in
the copied command. The token is intentionally visible in the command and
expires after the enrollment window.

The generated command has this shape:

```bash
(command -v curl >/dev/null 2>&1 && curl -fsSL https://github.com/wangxiangg1/vps-tool/releases/latest/download/install-agent.sh || wget -qO- https://github.com/wangxiangg1/vps-tool/releases/latest/download/install-agent.sh) | sh -s -- --node-id 'node-id' --registration-token 'one-time-token' --wss-url 'wss://panel.example.com/agent' --xui-unit 'x-ui' --warp-adapter 'generic'
```

The installer accepts these command-line arguments as well as the equivalent
`VPS_AGENT_*` environment variables for manual or automated installation.

For manual or automated installation, export the one-time registration values
and run the installer as root:

```bash
export VPS_AGENT_NODE_ID="node-id-from-the-control-plane"
export VPS_AGENT_REGISTRATION_TOKEN="one-time-registration-token"
export VPS_AGENT_WSS_URL="wss://panel.example.com/agent"
export VPS_AGENT_XUI_UNIT="x-ui"
export VPS_AGENT_WARP_ADAPTER="generic"
installer_url="https://github.com/wangxiangg1/vps-tool/releases/latest/download/install-agent.sh"
if command -v curl >/dev/null 2>&1; then
  curl --fail --silent --show-error --location "$installer_url" -o /tmp/vps-tool-install.sh
else
  wget -qO /tmp/vps-tool-install.sh "$installer_url"
fi
chmod 0755 /tmp/vps-tool-install.sh
if [ "$(id -u)" -eq 0 ]; then
  /bin/sh /tmp/vps-tool-install.sh
else
  sudo --preserve-env=VPS_AGENT_NODE_ID,VPS_AGENT_REGISTRATION_TOKEN,VPS_AGENT_WSS_URL,VPS_AGENT_XUI_UNIT,VPS_AGENT_WARP_ADAPTER \
    /bin/sh /tmp/vps-tool-install.sh
fi
rm -f /tmp/vps-tool-install.sh
```

Minimal Alpine LXC images commonly open a root shell and do not include
`sudo` yet. In that case, export the same variables and invoke
`/bin/sh /tmp/vps-tool-install.sh` directly; the installer adds Alpine's `doas`
for the restricted Agent-to-Helper policy.

The installer detects `linux/amd64` or `linux/arm64`, downloads the matching
Agent and Helper, verifies both against the release checksum list, creates the
dedicated `vps-agent` user, and installs a fixed privilege rule. On systemd
hosts it uses `sudo` and enables `vps-agent.service`; on Alpine/OpenRC it uses
`doas`, installs
`/etc/init.d/vps-agent` and enables it in the `default` runlevel. On Alpine the
installer uses `apk` to add only missing `curl`, `doas`, `coreutils`, and CA
certificate packages. It keeps an existing `/etc/vps-agent/agent.json` during
upgrades, so a long-term credential is not replaced. The first successful
registration atomically replaces the one-time token in that file.

Alpine compatibility requires OpenRC and a working `sudo` policy. The Agent
and Helper binaries are static Linux builds, but WARP through `wgcf` still
requires the container to expose `/dev/net/tun` and `CAP_NET_ADMIN`. An LXC
container without those privileges can connect to the control plane and report
basic host state, but WARP changes will fail. The configured `x-ui` service
name is managed through systemd or OpenRC according to the host.

Status collection probes both `api.ipify.org` and `api6.ipify.org`. When IPv6
is available, `change_ip` compares the WARP IPv6 exit address first because
WireGuard WARP IPv4 addresses may remain stable across reconnects; it falls
back to IPv4 when no IPv6 address is available.

The service user can invoke only `/usr/local/libexec/vps-agent-helper` through
`sudo` or `doas`; the
Helper accepts only the documented fixed argument forms. The Helper itself is
root-owned and validates every external executable path, service name, adapter,
watchdog token, deadline, and output. It supports `warp-cli`, `wgcf` via
`wg-quick`, `warp-go` via systemd or OpenRC, and `generic` auto-detection.

## Fixed Helper Contract

The helper path is a single absolute local path. The agent validates that it is
a regular executable file, not group/other writable, and root-owned on Linux.
The agent calls it with an argument vector only; it never invokes a shell,
interpolates command text, or accepts a path/script from the server.

The helper contract is:

```text
warp <adapter> status
warp <adapter> on
warp <adapter> off
ip <adapter>
watchdog <adapter> arm <random-token> <deadline-unix> <max-attempts>
watchdog <adapter> disarm <random-token>
xui <validated-unit> status
xui <validated-unit> restart
```

`warp ... status` returns bounded JSON with `state` (`on`, `off`, `degraded`,
or `unknown`) and optional `ipv4`/`ipv6`. `xui ... status` returns bounded JSON
with `state` (`running`, `stopped`, `failed`, `not_found`, or `unknown`). Any
missing helper is reported as `helper_not_found`; a non-zero exit or malformed
output is `helper_failed`.

The release installer places the Helper as a root-owned executable and grants
the Agent user a single `sudo -n` rule for that path. The Agent never invokes
a service manager, a shell, an interpreter, or arbitrary arguments itself. A
finite systemd transient unit runs the watchdog independently of the WSS
process on systemd hosts. On OpenRC hosts the Helper starts a detached,
token-keyed child under `/run/vps-agent/watchdogs` and checks its command line
before terminating it.

## Service installation

### systemd installation

Create `/etc/vps-agent/agent.json` as root with mode `0600`, install the binary
as `/usr/local/bin/vps-agent` owned by root, and run it with a dedicated low
privilege user. Create `/var/lib/vps-agent` owned by that user with mode `0700`.

```ini
[Unit]
Description=vps-tool low-resource agent
After=network-online.target
Wants=network-online.target

[Service]
User=vps-agent
Group=vps-agent
ExecStart=/usr/local/bin/vps-agent -config /etc/vps-agent/agent.json
Restart=always
RestartSec=5
PrivateTmp=true
ProtectSystem=strict
ProtectHome=true
ReadWritePaths=/etc/vps-agent /var/lib/vps-agent
NoNewPrivileges=false
PrivateDevices=false

[Install]
WantedBy=multi-user.target
```

Install and enable with `systemctl daemon-reload`, `systemctl enable --now
vps-agent.service`, then verify `systemctl is-active vps-agent.service`.
`NoNewPrivileges=false` is deliberate: the service uses a `sudo -n` rule that
allows only the installed fixed Helper. Do not broaden that rule to
`systemctl`, a shell, an interpreter, or arbitrary command arguments.

### Alpine/OpenRC installation

The installer creates `/etc/init.d/vps-agent`, runs the Agent as the dedicated
`vps-agent` user, and enables it in the default runlevel:

```bash
rc-update add vps-agent default
rc-service vps-agent start
rc-service vps-agent status
```

The fixed Helper uses `rc-service` for `x-ui` and `warp-go`. For WireGuard mode,
the `generic` adapter detects both the standard `wgcf` interface/configuration
and the fscarmen WARP script's `warp` interface/configuration. Its finite WARP
recovery watchdog does not require `systemd-run`; it uses the root-owned,
identity-checked child process described above.

## Actions and state

Only these actions are accepted: `get_status`, `get_ip`, `warp_on`, `warp_off`,
`change_ip`, `restart_xui`, and `upgrade_agent`. Parameters are strict JSON objects with no
unknown fields. Same-node state-changing actions are serialized; status
collection can run concurrently. Duplicate `request_id` values return the
persisted terminal result and never execute a second time.

`change_ip` runs entirely locally after acceptance. It requires WARP `on` or
`degraded`, obtains the old IP, arms a finite recovery watchdog, performs up to
three stop/start attempts, compares the new IP, and always attempts to leave
WARP ON even after a failure or WSS disconnect. The terminal result records the
stage-specific code, attempt count, old/new IP, final WARP state, and recovery
errors.

`upgrade_agent` is a fixed Helper operation, not a shell or URL parameter. It
resolves the latest numeric version from `wangxiangg1/vps-tool`, accepts only
the matching Linux amd64/arm64 release assets, verifies both binaries against
the release `SHA256SUMS`, rejects downgrades, and atomically replaces the Agent
and Helper with rollback copies. A detached fixed Helper process restarts only
the `vps-agent` service after the terminal result has been persisted.

The request journal is a bounded JSON file (256 entries/512 KiB in the binary)
with atomic replacement and mode `0600`. It is loaded at startup, so recent
terminal results survive an Agent restart and are replayed after reconnect.
Status collection reads bounded `/proc`, `/sys`, and filesystem statistics and
does not run a high-frequency polling loop.

## Security boundary

Production WSS TLS certificate verification is mandatory. URLs with embedded
credentials, queries, or fragments are rejected. Unknown protocol versions,
message fields, actions, parameters, unit names, helper paths, and helper JSON
fields fail closed. No remote shell, script upload, arbitrary file read/write,
systemd argument forwarding, user-supplied download URL, or user-supplied
executable loading is exposed.

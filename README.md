# vps-tool

Secure, lightweight VPS control plane with a FastAPI server, a low-memory Go
Agent, and a static operations console.

## Repository layout

- `server/`: FastAPI control plane, SQLite persistence, authentication, task
  scheduler, and Agent WebSocket gateway.
- `agent/`: Go Agent for Linux VPS nodes. It reports status and executes only
  the fixed Action allowlist; it never accepts remote shell text.
- `web/`: no-build browser console served by the control plane.
- `server/Dockerfile` and `docker-compose.yml`: control-plane container image
  and single-container VPS deployment.

## Local verification

Server prerequisites are Python 3.11+ and the dependencies in
`server/requirements.txt`. Agent prerequisites are Go 1.23+.

```powershell
python -m pip install -r server/requirements.txt
$env:VPS_TOOL_ADMIN_USER = "admin"
$env:VPS_TOOL_ADMIN_PASSWORD = "use-a-password-at-least-12-bytes"
$env:VPS_TOOL_COOKIE_SECURE = "false"
$env:VPS_TOOL_DB_PATH = ".\server\data\vps-tool.sqlite3"
python -m uvicorn app.main:app --app-dir server --host 127.0.0.1 --port 8000 --workers 1
```

Then open `http://127.0.0.1:8000/`. Agent installation and the fixed Helper
contract are documented in `agent/README.md`.

## Docker deployment

The GitHub Actions workflow publishes a multi-architecture control-plane image
to `ghcr.io/wangxiangg1/vps-tool` on pushes to `main` and version tags. On the
control VPS, copy the example environment file, replace the administrator
credentials, and start the service:

```bash
cp .env.example .env
${EDITOR:-vi} .env
docker compose pull
docker compose up -d
docker compose ps
```

The Compose file stores SQLite data in the named `vps-tool-data` volume and
binds port `8000` to `127.0.0.1` by default, so an HTTPS reverse proxy can be
placed in front of it. Keep `VPS_TOOL_COOKIE_SECURE=true` when serving through
HTTPS; only set it to `false` for local plain-HTTP testing. Set
`VPS_TOOL_IMAGE` to a version tag or digest when pinning deployments.

The `vps-tool` GHCR package is currently Public, so a public VPS deployment can
pull it without `docker login ghcr.io`. If package visibility is changed later,
set it back to Public in the package settings before using anonymous pulls.

## GitHub automation

Every push and pull request runs Python, frontend, and Go verification through
`.github/workflows/ci.yml`. Pushing a tag such as `v0.1.0` builds Linux amd64
and arm64 Agent binaries, generates SHA256 checksums, and creates a GitHub
Release through `.github/workflows/release-agent.yml`. Pushes to `main` and
version tags also build and publish the multi-architecture control-plane image
through `.github/workflows/publish-control-plane.yml`.

The control plane requires HTTPS/WSS in production. The public one-line Agent
installer and production WARP/x-ui Helper remain separate deployment work.

# vps-tool

Secure, lightweight VPS control plane with a FastAPI server, a low-memory Go
Agent, and a static operations console.

## Repository layout

- `server/`: FastAPI control plane, SQLite persistence, authentication, task
  scheduler, and Agent WebSocket gateway.
- `agent/`: Go Agent for Linux VPS nodes. It reports status and executes only
  the fixed Action allowlist; it never accepts remote shell text.
- `web/`: no-build browser console served by the control plane.
- `RPD.md`: product and security requirements.

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

## GitHub automation

Every push and pull request runs Python, frontend, and Go verification through
`.github/workflows/ci.yml`. Pushing a tag such as `v0.1.0` builds Linux amd64
and arm64 Agent binaries, generates SHA256 checksums, and creates a GitHub
Release through `.github/workflows/release-agent.yml`.

The control plane currently requires HTTPS/WSS in production. Docker Compose
deployment and the public one-line Agent installer are the next packaging
layer; the protocol and release artifact boundary are already defined here.

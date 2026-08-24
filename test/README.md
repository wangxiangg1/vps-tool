# Local panel demo

This directory contains disposable local demo tooling. It creates a SQLite
database, starts the control plane, and connects two mock Agents over local
WebSockets. No VPS or production database is used.

Start the demo from the project root:

```powershell
pwsh -File .\test\start-demo.ps1
```

Open `http://127.0.0.1:8000/` with:

- User: `demo`
- Password: `demo-local-panel-2026`

Stop only the demo processes with:

```powershell
pwsh -File .\test\stop-demo.ps1
```

All generated data and logs are under `test/runtime/` and are ignored by Git.
The folder can be removed after stopping the demo.

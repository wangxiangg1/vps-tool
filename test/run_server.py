from __future__ import annotations

import argparse
import os
import sys
from pathlib import Path


def main() -> None:
    parser = argparse.ArgumentParser(description="Start an isolated vps-tool demo server")
    parser.add_argument("--db", required=True, type=Path)
    parser.add_argument("--port", required=True, type=int)
    args = parser.parse_args()

    project_root = Path(__file__).resolve().parents[1]
    sys.path.insert(0, str(project_root / "server"))
    os.environ["VPS_TOOL_ADMIN_USER"] = "demo"
    os.environ["VPS_TOOL_ADMIN_PASSWORD"] = "demo-local-panel-2026"
    os.environ["VPS_TOOL_COOKIE_SECURE"] = "false"
    os.environ["VPS_TOOL_DB_PATH"] = str(args.db.resolve())
    os.environ["VPS_TOOL_HEARTBEAT_TIMEOUT"] = "90"
    os.environ["VPS_TOOL_SCHEDULER_INTERVAL"] = "5"

    import uvicorn

    from app.main import app

    uvicorn.run(app, host="127.0.0.1", port=args.port, workers=1)


if __name__ == "__main__":
    main()

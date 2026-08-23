from __future__ import annotations

import os
from dataclasses import dataclass
from pathlib import Path


class StartupConfigurationError(RuntimeError):
    """Raised when starting without a safe administrator configuration."""


def _env_bool(name: str, default: bool) -> bool:
    value = os.getenv(name)
    if value is None:
        return default
    normalized = value.strip().lower()
    if normalized in {"1", "true", "yes", "on"}:
        return True
    if normalized in {"0", "false", "no", "off"}:
        return False
    raise StartupConfigurationError(f"{name} must be a boolean value")


@dataclass(frozen=True)
class Settings:
    db_path: Path
    admin_user: str
    admin_password: str
    session_ttl_seconds: int = 28800
    cookie_secure: bool = True
    cookie_name: str = "vps_tool_session"
    protocol_version: int = 1
    heartbeat_timeout_seconds: int = 90
    scheduler_poll_seconds: int = 5
    scheduler_lease_seconds: int = 20
    enrollment_ttl_seconds: int = 600
    action_ttl_seconds: int = 180
    scheduler_interval_seconds: int = 5

    @classmethod
    def from_env(cls) -> "Settings":
        admin_user = os.getenv("VPS_TOOL_ADMIN_USER", "").strip()
        admin_password = os.getenv("VPS_TOOL_ADMIN_PASSWORD", "")
        if not admin_user or not admin_password:
            raise StartupConfigurationError(
                "VPS_TOOL_ADMIN_USER and VPS_TOOL_ADMIN_PASSWORD must be configured; "
                "the server refuses to start without an initial administrator credential"
            )
        password_size = len(admin_password.encode("utf-8"))
        if password_size < 12:
            raise StartupConfigurationError("VPS_TOOL_ADMIN_PASSWORD must contain at least 12 bytes")
        if password_size > 72:
            raise StartupConfigurationError("VPS_TOOL_ADMIN_PASSWORD must contain at most 72 UTF-8 bytes")
        try:
            session_ttl = max(300, int(os.getenv("VPS_TOOL_SESSION_TTL_SECONDS", "28800")))
            enrollment_ttl = max(60, int(os.getenv("VPS_TOOL_ENROLLMENT_TTL", "600")))
            action_ttl = max(30, int(os.getenv("VPS_TOOL_ACTION_TTL", "180")))
            scheduler_interval = max(1, int(os.getenv("VPS_TOOL_SCHEDULER_INTERVAL", "5")))
            heartbeat_timeout = max(30, int(os.getenv("VPS_TOOL_HEARTBEAT_TIMEOUT", "90")))
        except ValueError as exc:
            raise StartupConfigurationError("numeric VPS_TOOL_* settings must be integers") from exc
        raw_path = os.getenv("VPS_TOOL_DB_PATH", "./data/vps-tool.sqlite3")
        db_path = Path(raw_path).expanduser()
        if not db_path.is_absolute():
            db_path = Path.cwd() / db_path
        return cls(
            db_path=db_path,
            admin_user=admin_user,
            admin_password=admin_password,
            session_ttl_seconds=session_ttl,
            cookie_secure=_env_bool("VPS_TOOL_COOKIE_SECURE", True),
            cookie_name=os.getenv("VPS_TOOL_SESSION_COOKIE", "vps_tool_session"),
            heartbeat_timeout_seconds=heartbeat_timeout,
            scheduler_poll_seconds=scheduler_interval,
            scheduler_lease_seconds=max(10, scheduler_interval * 3),
            enrollment_ttl_seconds=enrollment_ttl,
            action_ttl_seconds=action_ttl,
            scheduler_interval_seconds=scheduler_interval,
        )

    def configuration_error(self) -> str | None:
        return None

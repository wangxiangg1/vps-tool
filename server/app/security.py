from __future__ import annotations

import hashlib
import hmac
import secrets
import threading
import uuid
from datetime import timedelta
from typing import Any

import bcrypt
from fastapi import Depends, HTTPException, Request, status

from .config import Settings
from .db import Database
from .timeutil import iso_now, parse_iso, utc_now


SESSION_COOKIE = "vps_tool_session"
CSRF_HEADER = "X-CSRF-Token"
_DUMMY_PASSWORD_HASH = bcrypt.hashpw(
    b"vps-tool-invalid-login-placeholder",
    bcrypt.gensalt(rounds=12),
)


def random_token(prefix: str) -> str:
    return f"{prefix}_{secrets.token_urlsafe(32)}"


def hash_token(value: str) -> str:
    return hashlib.sha256(value.encode("utf-8")).hexdigest()


def verify_token(value: str, expected_hash: str) -> bool:
    return hmac.compare_digest(hash_token(value), expected_hash)


def hash_password(value: str) -> str:
    return bcrypt.hashpw(value.encode("utf-8"), bcrypt.gensalt()).decode("ascii")


def verify_password(value: str, expected_hash: str) -> bool:
    try:
        return bcrypt.checkpw(value.encode("utf-8"), expected_hash.encode("ascii"))
    except (ValueError, TypeError):
        return False


class LoginRateLimiter:
    def __init__(self, max_failures: int = 5, window_seconds: int = 60, lockout_seconds: int = 300):
        self.max_failures = max_failures
        self.window_seconds = window_seconds
        self.lockout_seconds = lockout_seconds
        self._lock = threading.Lock()
        self._failures: dict[str, list[Any]] = {}

    def is_blocked(self, key: str) -> bool:
        now = utc_now()
        with self._lock:
            entries = self._failures.get(key, [])
            entries = [entry for entry in entries if now - entry < timedelta(seconds=self.lockout_seconds)]
            self._failures[key] = entries
            return len(entries) >= self.max_failures

    def record_failure(self, key: str) -> None:
        now = utc_now()
        with self._lock:
            entries = self._failures.setdefault(key, [])
            entries[:] = [entry for entry in entries if now - entry < timedelta(seconds=self.lockout_seconds)]
            entries.append(now)

    def clear(self, key: str) -> None:
        with self._lock:
            self._failures.pop(key, None)


def ensure_admin(db: Database, settings: Settings) -> None:
    existing = db.fetchone("SELECT id FROM users LIMIT 1")
    if existing:
        return
    now = iso_now()
    db.execute(
        """
        INSERT INTO users (id, username, password_hash, created_at, updated_at)
        VALUES (?, ?, ?, ?, ?)
        """,
        (str(uuid.uuid4()), settings.admin_user, hash_password(settings.admin_password), now, now),
    )


def authenticate_admin(db: Database, username: str, password: str) -> dict[str, Any] | None:
    row = db.fetchone(
        "SELECT id, username, password_hash FROM users WHERE username = ?",
        (username,),
    )
    expected_hash = row["password_hash"] if row else _DUMMY_PASSWORD_HASH.decode("ascii")
    if not verify_password(password, expected_hash):
        return None
    return {"id": row["id"], "username": row["username"]} if row else None


def create_session(db: Database, user_id: str, ttl_seconds: int) -> tuple[str, str, str]:
    session_token = random_token("sess")
    csrf_token = random_token("csrf")
    now = utc_now()
    expires_at = now + timedelta(seconds=ttl_seconds)
    db.execute(
        """
        INSERT INTO sessions
          (id, user_id, session_hash, csrf_hash, created_at, expires_at, last_seen_at)
        VALUES (?, ?, ?, ?, ?, ?, ?)
        """,
        (
            str(uuid.uuid4()),
            user_id,
            hash_token(session_token),
            hash_token(csrf_token),
            iso_now(),
            expires_at.isoformat().replace("+00:00", "Z"),
            iso_now(),
        ),
    )
    return session_token, csrf_token, expires_at.isoformat().replace("+00:00", "Z")


def rotate_csrf_token(db: Database, session: dict[str, Any]) -> str:
    """Issue a fresh CSRF token without exposing the stored hash."""
    token = random_token("csrf")
    db.rotate_csrf(session["session_hash"], hash_token(token))
    return token


def get_session(db: Database, session_token: str | None) -> dict[str, Any] | None:
    if not session_token:
        return None
    row = db.fetchone(
        """
        SELECT s.id, s.user_id, s.session_hash, s.csrf_hash, s.expires_at, u.username
        FROM sessions s JOIN users u ON u.id = s.user_id
        WHERE s.session_hash = ?
        """,
        (hash_token(session_token),),
    )
    if not row:
        return None
    expires_at = parse_iso(row["expires_at"])
    if not expires_at or expires_at <= utc_now():
        db.execute("DELETE FROM sessions WHERE id = ?", (row["id"],))
        return None
    db.execute(
        "UPDATE sessions SET last_seen_at = ? WHERE id = ?",
        (iso_now(), row["id"]),
    )
    return dict(row)


def delete_session(db: Database, session_token: str | None) -> None:
    if session_token:
        db.execute("DELETE FROM sessions WHERE session_hash = ?", (hash_token(session_token),))


async def require_session(request: Request) -> dict[str, Any]:
    db: Database = request.app.state.db
    cookie_name = getattr(request.app.state.settings, "cookie_name", SESSION_COOKIE)
    session = get_session(db, request.cookies.get(cookie_name))
    if not session:
        raise HTTPException(status_code=status.HTTP_401_UNAUTHORIZED, detail="authentication_required")
    return session


async def require_csrf(
    request: Request,
    session: dict[str, Any] = Depends(require_session),
) -> dict[str, Any]:
    supplied = request.headers.get(CSRF_HEADER, "")
    if not supplied or not verify_token(supplied, session["csrf_hash"]):
        raise HTTPException(status_code=status.HTTP_403_FORBIDDEN, detail="csrf_failed")
    return session

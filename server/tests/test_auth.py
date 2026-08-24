from __future__ import annotations

import asyncio
import os
import unittest
import uuid
from pathlib import Path

from app.config import Settings
from app.db import Database

os.environ.setdefault("VPS_TOOL_ADMIN_USER", "test-bootstrap")
os.environ.setdefault("VPS_TOOL_ADMIN_PASSWORD", "test-bootstrap-password")

from app.main import build_app
from app.schemas import ChangePasswordRequest
from app.security import authenticate_admin, create_session, ensure_admin, get_session


class PasswordChangeTest(unittest.TestCase):
    def setUp(self) -> None:
        self.database_path = Path("server/data") / f"auth-test-{uuid.uuid4()}.sqlite3"
        self.settings = Settings(
            db_path=self.database_path,
            admin_user="admin",
            admin_password="old-password-123",
            cookie_secure=False,
        )
        self.database = Database(self.database_path)
        self.database.initialize()
        ensure_admin(self.database, self.settings)
        self.app = build_app(self.settings)

    def tearDown(self) -> None:
        for suffix in ("", "-wal", "-shm"):
            (Path(str(self.database_path) + suffix)).unlink(missing_ok=True)

    def test_change_password_invalidates_sessions(self) -> None:
        admin = self.database.fetchone("SELECT id FROM users WHERE username = ?", ("admin",))
        self.assertIsNotNone(admin)
        session_token, _, _ = create_session(self.database, admin["id"], 3600)
        session = get_session(self.database, session_token)
        self.assertIsNotNone(session)

        route = next(route for route in self.app.routes if route.path == "/api/auth/change-password")
        response = asyncio.run(
            route.endpoint(
                ChangePasswordRequest(
                    current_password="old-password-123",
                    new_password="new-password-456",
                    confirm_password="new-password-456",
                ),
                session=session,
            )
        )

        self.assertEqual(response, {"status": "password_changed"})
        self.assertIsNone(authenticate_admin(self.database, "admin", "old-password-123"))
        self.assertIsNotNone(authenticate_admin(self.database, "admin", "new-password-456"))
        self.assertEqual(self.database.fetchone("SELECT COUNT(*) FROM sessions")[0], 0)
        self.assertEqual(
            self.database.fetchone(
                "SELECT COUNT(*) FROM audit_logs WHERE event_type = 'password_changed'"
            )[0],
            1,
        )


if __name__ == "__main__":
    unittest.main()

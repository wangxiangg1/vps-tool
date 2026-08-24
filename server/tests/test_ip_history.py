from __future__ import annotations

import unittest
import uuid
from pathlib import Path

from app.config import Settings
from app.db import Database, SCHEMA_VERSION
from app.nodes import NodeService
from app.timeutil import iso_now


class IPHistoryTest(unittest.TestCase):
    def setUp(self) -> None:
        path = Path("server/data") / f"history-test-{uuid.uuid4()}.sqlite3"
        path.parent.mkdir(parents=True, exist_ok=True)
        self.database_path = path
        self.database = Database(path)
        self.database.initialize()
        settings = Settings(db_path=path, admin_user="test", admin_password="test-password-123")
        self.nodes = NodeService(self.database, settings)
        self.node, _, _ = self.nodes.create(
            name="test-node",
            region="test",
            tags=[],
            warp_adapter="generic",
            xui_service="x-ui",
            notes="",
            actor_id=None,
        )

    def tearDown(self) -> None:
        for suffix in ("", "-wal", "-shm"):
            (Path(str(self.database_path) + suffix)).unlink(missing_ok=True)

    def test_schema_and_history_retention(self) -> None:
        self.assertEqual(self.database.fetchone("PRAGMA user_version")[0], SCHEMA_VERSION)
        now = iso_now()
        for index in range(105):
            request_id = f"request-{index:03d}"
            self.database.execute(
                """
                INSERT INTO action_requests
                  (id, node_id, action, parameters_json, source, status, attempts,
                   issued_at, deadline_at, finished_at, created_at, updated_at)
                VALUES (?, ?, 'change_ip', '{}', 'manual', 'succeeded', 1,
                        ?, ?, ?, ?, ?)
                """,
                (request_id, self.node["id"], now, now, now, now, now),
            )
            self.nodes.record_ip_change(
                self.node["id"],
                request_id,
                {
                    "old_ip": f"2001:db8::{index}",
                    "new_ip": f"2001:db8::{index + 1}",
                    "attempts": 1,
                    "duration_ms": 1000,
                },
                True,
            )

        history = self.nodes.list_ip_changes(self.node["id"])
        self.assertIsNotNone(history)
        self.assertEqual(len(history), 100)
        self.assertEqual(
            self.database.fetchone(
                "SELECT COUNT(*) FROM ip_change_history WHERE node_id = ?",
                (self.node["id"],),
            )[0],
            100,
        )
        self.assertEqual(self.nodes.get(self.node["id"])["public_ipv6"], "2001:db8::105")


if __name__ == "__main__":
    unittest.main()

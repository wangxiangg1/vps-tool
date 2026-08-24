from __future__ import annotations

import hashlib
import unittest
import uuid
from pathlib import Path

from app.backups import XUIBackupService
from app.config import Settings
from app.db import Database
from app.nodes import NodeService


class XUIBackupTest(unittest.TestCase):
    def setUp(self) -> None:
        root = Path("server/data")
        self.database_path = root / f"backup-test-{uuid.uuid4()}.sqlite3"
        self.backup_dir = root / f"backup-files-{uuid.uuid4()}"
        self.database = Database(self.database_path)
        self.database.initialize()
        settings = Settings(
            db_path=self.database_path,
            backup_dir=self.backup_dir,
            admin_user="test",
            admin_password="test-password-123",
        )
        self.nodes = NodeService(self.database, settings)
        self.node, _, _ = self.nodes.create(
            name="Singapore Egress / 03",
            region="SG",
            tags=[],
            warp_adapter="generic",
            xui_service="x-ui",
            notes="",
            actor_id=None,
        )
        self.backups = XUIBackupService(self.database, settings, self.nodes)

    def tearDown(self) -> None:
        for suffix in ("", "-wal", "-shm"):
            (Path(str(self.database_path) + suffix)).unlink(missing_ok=True)
        for path in sorted(self.backup_dir.glob("**/*"), reverse=True) if self.backup_dir.exists() else []:
            if path.is_file():
                path.unlink()
            elif path.is_dir():
                path.rmdir()
        self.backup_dir.rmdir() if self.backup_dir.exists() else None

    def test_online_upload_metadata_and_delete(self) -> None:
        pending = self.backups.prepare(self.node["id"])
        self.assertEqual(pending["status"], "pending")
        self.assertIn("Singapore-Egress-03", pending["filename"])

        payload = b"SQLite format 3\x00test backup"
        ready = self.backups.save_bytes(pending["id"], self.node["id"], payload)
        self.assertEqual(ready["status"], "ready")
        self.assertEqual(ready["size_bytes"], len(payload))
        self.assertEqual(ready["sha256"], hashlib.sha256(payload).hexdigest())
        row, path = self.backups.ready_path(pending["id"], self.node["id"])
        self.assertEqual(row["id"], pending["id"])
        self.assertEqual(path.read_bytes(), payload)
        self.assertTrue(self.backups.delete(pending["id"], self.node["id"]))
        with self.assertRaises(ValueError):
            self.backups.ready_path(pending["id"], self.node["id"])


if __name__ == "__main__":
    unittest.main()

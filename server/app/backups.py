from __future__ import annotations

import hashlib
import re
import uuid
from pathlib import Path
from typing import Any

from .config import Settings
from .db import Database
from .nodes import NodeService
from .timeutil import iso_now


MAX_BACKUP_BYTES = 32 * 1024 * 1024
_FILENAME_CHARS = re.compile(r"[^A-Za-z0-9._-]+")


class BackupError(ValueError):
    def __init__(self, code: str, message: str):
        self.code = code
        self.message = message
        super().__init__(message)


def _safe_name(value: str) -> str:
    value = _FILENAME_CHARS.sub("-", value.strip()).strip(".-_")
    return value[:80] or "node"


class XUIBackupService:
    def __init__(self, db: Database, settings: Settings, nodes: NodeService):
        self.db = db
        self.settings = settings
        self.nodes = nodes

    def _row(self, backup_id: str) -> Any | None:
        return self.db.fetchone("SELECT * FROM xui_backups WHERE id = ?", (backup_id,))

    @staticmethod
    def _as_dict(row: Any) -> dict[str, Any]:
        value = dict(row)
        value["size_bytes"] = int(value.get("size_bytes") or 0)
        return value

    def prepare(self, node_id: str, actor_id: str | None = None) -> dict[str, Any]:
        node = self.nodes.get(node_id)
        if not node:
            raise BackupError("node_not_found", "node was not found")
        backup_id = str(uuid.uuid4())
        now = iso_now()
        relative_path = f"{node_id}/{backup_id}.db"
        filename = f"{_safe_name(node['name'])}_{now.replace(':', '').replace('Z', '')}_x-ui.db"
        self.db.execute(
            """
            INSERT INTO xui_backups
              (id, node_id, filename, relative_path, size_bytes, status, created_at, updated_at)
            VALUES (?, ?, ?, ?, 0, 'pending', ?, ?)
            """,
            (backup_id, node_id, filename, relative_path, now, now),
        )
        return self._as_dict(self._row(backup_id))

    def list(self, node_id: str) -> list[dict[str, Any]]:
        return [
            self._as_dict(row)
            for row in self.db.fetchall(
                "SELECT * FROM xui_backups WHERE node_id = ? ORDER BY created_at DESC LIMIT 50",
                (node_id,),
            )
        ]

    def get_for_node(self, backup_id: str, node_id: str) -> dict[str, Any] | None:
        row = self._row(backup_id)
        if not row or row["node_id"] != node_id:
            return None
        return self._as_dict(row)

    def path_for(self, row: Any) -> Path:
        path = (self.settings.backup_dir / row["relative_path"]).resolve()
        root = self.settings.backup_dir.resolve()
        if root != path and root not in path.parents:
            raise BackupError("backup_path_invalid", "backup path escaped the configured directory")
        return path

    def save_bytes(self, backup_id: str, node_id: str, payload: bytes) -> dict[str, Any]:
        if not payload or len(payload) > MAX_BACKUP_BYTES:
            raise BackupError("backup_too_large", "x-ui backup exceeds the 32 MiB limit")
        row = self.get_for_node(backup_id, node_id)
        if not row:
            raise BackupError("backup_not_found", "backup request was not found")
        path = self.path_for(row)
        path.parent.mkdir(parents=True, exist_ok=True)
        temporary = path.with_name(f".{path.name}.{uuid.uuid4().hex}.part")
        try:
            temporary.write_bytes(payload)
            temporary.chmod(0o600)
            temporary.replace(path)
        finally:
            if temporary.exists():
                temporary.unlink()
        digest = hashlib.sha256(payload).hexdigest()
        now = iso_now()
        self.db.execute(
            """
            UPDATE xui_backups
            SET size_bytes = ?, sha256 = ?, status = 'ready', updated_at = ?
            WHERE id = ? AND node_id = ?
            """,
            (len(payload), digest, now, backup_id, node_id),
        )
        return self._as_dict(self._row(backup_id))

    def ready_path(self, backup_id: str, node_id: str) -> tuple[dict[str, Any], Path]:
        row = self.get_for_node(backup_id, node_id)
        if not row or row["status"] != "ready":
            raise BackupError("backup_not_ready", "x-ui backup is not ready")
        path = self.path_for(row)
        if not path.is_file():
            raise BackupError("backup_missing", "x-ui backup file is missing")
        return row, path

    def delete(self, backup_id: str, node_id: str) -> bool:
        row = self.get_for_node(backup_id, node_id)
        if not row:
            return False
        path = self.path_for(row)
        self.db.execute("DELETE FROM xui_backups WHERE id = ? AND node_id = ?", (backup_id, node_id))
        if path.exists():
            path.unlink()
        return True

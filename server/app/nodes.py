from __future__ import annotations

import json
import re
import uuid
from datetime import timedelta
from typing import Any

from .config import Settings
from .db import Database
from .security import hash_token, random_token
from .timeutil import iso_now, parse_iso, to_iso, utc_now


XUI_SERVICE_RE = re.compile(r"^[A-Za-z0-9][A-Za-z0-9_.@:-]{0,127}$")
NODE_STATUS_FIELDS = {
    "agent_version",
    "hostname",
    "os_name",
    "os_version",
    "architecture",
    "cpu_percent",
    "memory_used_bytes",
    "memory_total_bytes",
    "root_used_bytes",
    "root_total_bytes",
    "uptime_seconds",
    "warp_status",
    "xui_status",
    "public_ipv4",
    "public_ipv6",
}


class NodeValidationError(ValueError):
    pass


def validate_xui_service(value: str) -> str:
    if not XUI_SERVICE_RE.fullmatch(value):
        raise NodeValidationError(
            "xui_service must be a systemd unit or OpenRC service name without spaces, paths, or shell characters"
        )
    return value


def _decode_json(value: str, default: Any) -> Any:
    try:
        return json.loads(value)
    except (TypeError, ValueError):
        return default


def node_to_dict(row: Any, online: bool | None = None) -> dict[str, Any]:
    result = dict(row)
    result["tags"] = _decode_json(result.pop("tags_json", "[]"), [])
    result["status"] = _decode_json(result.pop("status_json", "{}"), {})
    result["last_ip_change_result"] = _decode_json(
        result.pop("last_ip_change_result_json", "null"), None
    )
    result.pop("deleted_at", None)
    if online is not None:
        result["online"] = online
    return result


def _audit_values(
    *,
    audit_id: str,
    actor_type: str,
    actor_id: str | None,
    event_type: str,
    node_id: str | None,
    metadata: dict[str, Any],
    created_at: str,
) -> tuple[Any, ...]:
    return (
        audit_id,
        actor_type,
        actor_id,
        event_type,
        node_id,
        json.dumps(metadata, separators=(",", ":"), ensure_ascii=True),
        created_at,
    )


class NodeService:
    def __init__(self, db: Database, settings: Settings):
        self.db = db
        self.settings = settings

    def list_nodes(self, online_ids: set[str] | None = None) -> list[dict[str, Any]]:
        rows = self.db.fetchall(
            "SELECT * FROM nodes WHERE deleted_at IS NULL ORDER BY name COLLATE NOCASE, id"
        )
        return [node_to_dict(row, online=(row["id"] in online_ids) if online_ids is not None else None) for row in rows]

    def get(self, node_id: str, include_deleted: bool = False) -> dict[str, Any] | None:
        query = "SELECT * FROM nodes WHERE id = ?"
        params: tuple[Any, ...] = (node_id,)
        if not include_deleted:
            query += " AND deleted_at IS NULL"
        row = self.db.fetchone(query, params)
        return node_to_dict(row) if row else None

    def create(
        self,
        *,
        name: str,
        region: str,
        tags: list[str],
        warp_adapter: str,
        xui_service: str,
        notes: str,
        actor_id: str | None,
    ) -> tuple[dict[str, Any], str, str]:
        validate_xui_service(xui_service)
        node_id = str(uuid.uuid4())
        token = random_token("enroll")
        now = utc_now()
        expires_at = now + timedelta(seconds=self.settings.enrollment_ttl_seconds)
        now_text = to_iso(now)
        expires_text = to_iso(expires_at)
        with self.db.transaction(immediate=True) as connection:
            connection.execute(
                """
                INSERT INTO nodes
                  (id, name, region, tags_json, warp_adapter, xui_service, notes,
                   created_at, updated_at)
                VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
                """,
                (
                    node_id,
                    name,
                    region,
                    json.dumps(tags, separators=(",", ":"), ensure_ascii=True),
                    warp_adapter,
                    xui_service,
                    notes,
                    now_text,
                    now_text,
                ),
            )
            connection.execute(
                """
                INSERT INTO enrollment_tokens
                  (id, node_id, token_hash, created_at, expires_at)
                VALUES (?, ?, ?, ?, ?)
                """,
                (str(uuid.uuid4()), node_id, hash_token(token), now_text, expires_text),
            )
            connection.execute(
                """
                INSERT INTO audit_logs
                  (id, actor_type, actor_id, event_type, node_id, metadata_json, created_at)
                VALUES (?, ?, ?, ?, ?, ?, ?)
                """,
                _audit_values(
                    audit_id=str(uuid.uuid4()),
                    actor_type="admin",
                    actor_id=actor_id,
                    event_type="node_created",
                    node_id=node_id,
                    metadata={"name": name},
                    created_at=now_text,
                ),
            )
        node = self.get(node_id)
        assert node is not None
        return node, token, expires_text

    def update(
        self,
        node_id: str,
        changes: dict[str, Any],
        actor_id: str | None,
    ) -> dict[str, Any] | None:
        existing = self.get(node_id)
        if not existing:
            return None
        if "xui_service" in changes:
            validate_xui_service(changes["xui_service"])
        if not changes:
            return existing
        allowed = {"name", "region", "tags", "warp_adapter", "xui_service", "notes"}
        unknown = set(changes) - allowed
        if unknown:
            raise NodeValidationError("unsupported node fields")
        columns: list[str] = []
        params: list[Any] = []
        for key, value in changes.items():
            column = "tags_json" if key == "tags" else key
            columns.append(f"{column} = ?")
            params.append(
                json.dumps(value, separators=(",", ":"), ensure_ascii=True)
                if key == "tags"
                else value
            )
        now = iso_now()
        columns.extend(["updated_at = ?"])
        params.extend([now, node_id])
        with self.db.transaction(immediate=True) as connection:
            cursor = connection.execute(
                f"UPDATE nodes SET {', '.join(columns)} WHERE id = ? AND deleted_at IS NULL",
                tuple(params),
            )
            if cursor.rowcount == 0:
                return None
            connection.execute(
                """
                INSERT INTO audit_logs
                  (id, actor_type, actor_id, event_type, node_id, metadata_json, created_at)
                VALUES (?, ?, ?, ?, ?, ?, ?)
                """,
                _audit_values(
                    audit_id=str(uuid.uuid4()),
                    actor_type="admin",
                    actor_id=actor_id,
                    event_type="node_updated",
                    node_id=node_id,
                    metadata={"fields": sorted(changes)},
                    created_at=now,
                ),
            )
        return self.get(node_id)

    def soft_delete(self, node_id: str, actor_id: str | None) -> bool:
        now = iso_now()
        with self.db.transaction(immediate=True) as connection:
            cursor = connection.execute(
                "UPDATE nodes SET deleted_at = ?, updated_at = ? WHERE id = ? AND deleted_at IS NULL",
                (now, now, node_id),
            )
            if cursor.rowcount == 0:
                return False
            connection.execute(
                """
                UPDATE agent_credentials
                SET status = 'revoked', revoked_at = ?
                WHERE node_id = ? AND status = 'active'
                """,
                (now, node_id),
            )
            connection.execute(
                "UPDATE tasks SET enabled = 0, updated_at = ? "
                "WHERE instr(node_ids_json, char(34) || ? || char(34)) > 0",
                (now, node_id),
            )
            connection.execute(
                """
                INSERT INTO audit_logs
                  (id, actor_type, actor_id, event_type, node_id, metadata_json, created_at)
                VALUES (?, ?, ?, ?, ?, ?, ?)
                """,
                _audit_values(
                    audit_id=str(uuid.uuid4()),
                    actor_type="admin",
                    actor_id=actor_id,
                    event_type="node_deleted",
                    node_id=node_id,
                    metadata={},
                    created_at=now,
                ),
            )
        return True

    def create_enrollment_token(self, node_id: str, actor_id: str | None) -> tuple[str, str] | None:
        if not self.get(node_id):
            return None
        token = random_token("enroll")
        now = utc_now()
        expires = now + timedelta(seconds=self.settings.enrollment_ttl_seconds)
        now_text = to_iso(now)
        expires_text = to_iso(expires)
        with self.db.transaction(immediate=True) as connection:
            connection.execute(
                "UPDATE enrollment_tokens SET used_at = ? WHERE node_id = ? AND used_at IS NULL",
                (now_text, node_id),
            )
            connection.execute(
                """
                INSERT INTO enrollment_tokens
                  (id, node_id, token_hash, created_at, expires_at)
                VALUES (?, ?, ?, ?, ?)
                """,
                (str(uuid.uuid4()), node_id, hash_token(token), now_text, expires_text),
            )
            connection.execute(
                """
                INSERT INTO audit_logs
                  (id, actor_type, actor_id, event_type, node_id, metadata_json, created_at)
                VALUES (?, ?, ?, ?, ?, ?, ?)
                """,
                _audit_values(
                    audit_id=str(uuid.uuid4()),
                    actor_type="admin",
                    actor_id=actor_id,
                    event_type="enrollment_token_created",
                    node_id=node_id,
                    metadata={"expires_at": expires_text},
                    created_at=now_text,
                ),
            )
        return token, expires_text

    def consume_enrollment_token(self, node_id: str, token: str) -> tuple[dict[str, Any], str] | None:
        now = utc_now()
        now_text = to_iso(now)
        token_hash = hash_token(token)
        raw_credential = random_token("agent")
        with self.db.transaction(immediate=True) as connection:
            row = connection.execute(
                """
                SELECT e.id, e.node_id, e.expires_at, n.deleted_at
                FROM enrollment_tokens e JOIN nodes n ON n.id = e.node_id
                WHERE e.node_id = ? AND e.token_hash = ? AND e.used_at IS NULL
                """,
                (node_id, token_hash),
            ).fetchone()
            if not row or row["deleted_at"] is not None:
                return None
            expires = parse_iso(row["expires_at"])
            if not expires or expires <= now:
                return None
            connection.execute(
                "UPDATE enrollment_tokens SET used_at = ? WHERE id = ? AND used_at IS NULL",
                (now_text, row["id"]),
            )
            connection.execute(
                """
                UPDATE agent_credentials
                SET status = 'revoked', revoked_at = ?
                WHERE node_id = ? AND status = 'active'
                """,
                (now_text, node_id),
            )
            connection.execute(
                """
                INSERT INTO agent_credentials
                  (id, node_id, credential_hash, status, created_at)
                VALUES (?, ?, ?, 'active', ?)
                """,
                (str(uuid.uuid4()), node_id, hash_token(raw_credential), now_text),
            )
            connection.execute(
                """
                INSERT INTO audit_logs
                  (id, actor_type, actor_id, event_type, node_id, metadata_json, created_at)
                VALUES (?, ?, ?, ?, ?, ?, ?)
                """,
                _audit_values(
                    audit_id=str(uuid.uuid4()),
                    actor_type="agent",
                    actor_id=None,
                    event_type="enrollment_token_used",
                    node_id=node_id,
                    metadata={},
                    created_at=now_text,
                ),
            )
        node = self.get(node_id)
        assert node is not None
        return node, raw_credential

    def authenticate_credential(self, node_id: str, credential: str) -> dict[str, Any] | None:
        row = self.db.fetchone(
            """
            SELECT n.*
            FROM nodes n JOIN agent_credentials c ON c.node_id = n.id
            WHERE n.id = ? AND n.deleted_at IS NULL AND c.status = 'active' AND c.credential_hash = ?
            """,
            (node_id, hash_token(credential)),
        )
        return node_to_dict(row) if row else None

    def rotate_credential(self, node_id: str, actor_id: str | None) -> str | None:
        if not self.get(node_id):
            return None
        raw_credential = random_token("agent")
        now = iso_now()
        with self.db.transaction(immediate=True) as connection:
            connection.execute(
                """
                UPDATE agent_credentials
                SET status = 'revoked', revoked_at = ?
                WHERE node_id = ? AND status = 'active'
                """,
                (now, node_id),
            )
            connection.execute(
                """
                INSERT INTO agent_credentials
                  (id, node_id, credential_hash, status, created_at)
                VALUES (?, ?, ?, 'active', ?)
                """,
                (str(uuid.uuid4()), node_id, hash_token(raw_credential), now),
            )
            connection.execute(
                """
                INSERT INTO audit_logs
                  (id, actor_type, actor_id, event_type, node_id, metadata_json, created_at)
                VALUES (?, ?, ?, ?, ?, ?, ?)
                """,
                _audit_values(
                    audit_id=str(uuid.uuid4()),
                    actor_type="admin",
                    actor_id=actor_id,
                    event_type="agent_credential_rotated",
                    node_id=node_id,
                    metadata={},
                    created_at=now,
                ),
            )
        return raw_credential

    def revoke_credentials(self, node_id: str, actor_id: str | None, reason: str) -> bool:
        now = iso_now()
        with self.db.transaction(immediate=True) as connection:
            if not connection.execute(
                "SELECT id FROM nodes WHERE id = ? AND deleted_at IS NULL", (node_id,)
            ).fetchone():
                return False
            connection.execute(
                """
                UPDATE agent_credentials
                SET status = 'revoked', revoked_at = ?
                WHERE node_id = ? AND status = 'active'
                """,
                (now, node_id),
            )
            connection.execute(
                """
                INSERT INTO audit_logs
                  (id, actor_type, actor_id, event_type, node_id, metadata_json, created_at)
                VALUES (?, ?, ?, ?, ?, ?, ?)
                """,
                _audit_values(
                    audit_id=str(uuid.uuid4()),
                    actor_type="admin",
                    actor_id=actor_id,
                    event_type="agent_credential_revoked",
                    node_id=node_id,
                    metadata={"reason": reason},
                    created_at=now,
                ),
            )
        return True

    def mark_seen(self, node_id: str, seen_at: str | None = None) -> None:
        value = seen_at or iso_now()
        self.db.execute(
            "UPDATE nodes SET last_seen_at = ?, updated_at = ? WHERE id = ? AND deleted_at IS NULL",
            (value, value, node_id),
        )

    def apply_status(self, node_id: str, payload: dict[str, Any], observed_at: str | None = None) -> None:
        now = iso_now()
        updates: list[str] = []
        params: list[Any] = []
        for key in NODE_STATUS_FIELDS:
            if key in payload:
                updates.append(f"{key} = ?")
                params.append(payload[key])
        updates.extend(["status_json = ?", "last_status_at = ?", "last_seen_at = ?", "updated_at = ?"])
        params.extend([
            json.dumps(payload, separators=(",", ":"), ensure_ascii=True),
            observed_at or now,
            now,
            now,
            node_id,
        ])
        with self.db.transaction(immediate=True) as connection:
            connection.execute(
                f"UPDATE nodes SET {', '.join(updates)} WHERE id = ? AND deleted_at IS NULL",
                tuple(params),
            )

    def record_ip_change(self, node_id: str, result: dict[str, Any], success: bool) -> None:
        now = iso_now()
        with self.db.transaction(immediate=True) as connection:
            connection.execute(
                """
                UPDATE nodes
                SET last_ip_change_at = ?, last_ip_change_result_json = ?, updated_at = ?
                WHERE id = ? AND deleted_at IS NULL
                """,
                (
                    now,
                    json.dumps({"success": success, **result}, separators=(",", ":"), ensure_ascii=True),
                    now,
                    node_id,
                ),
            )

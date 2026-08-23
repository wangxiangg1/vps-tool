from __future__ import annotations

import sqlite3
from contextlib import contextmanager
from pathlib import Path
from typing import Any, Iterator

from .utils import iso_now, json_dumps, json_loads, to_iso, utc_now


class DatabaseError(RuntimeError):
    pass


class NotFoundError(DatabaseError):
    pass


class ConflictError(DatabaseError):
    pass


class Database:
    """Small per-operation SQLite wrapper; every connection applies safety PRAGMAs."""

    CURRENT_SCHEMA_VERSION = 1

    def __init__(self, path: Path):
        self.path = path

    def connect(self) -> sqlite3.Connection:
        self.path.parent.mkdir(parents=True, exist_ok=True)
        connection = sqlite3.connect(
            self.path,
            timeout=5,
            isolation_level=None,
            check_same_thread=False,
        )
        connection.row_factory = sqlite3.Row
        connection.execute("PRAGMA foreign_keys = ON")
        connection.execute("PRAGMA busy_timeout = 5000")
        connection.execute("PRAGMA journal_mode = WAL")
        return connection

    @contextmanager
    def transaction(self, immediate: bool = False) -> Iterator[sqlite3.Connection]:
        connection = self.connect()
        try:
            connection.execute("BEGIN IMMEDIATE" if immediate else "BEGIN")
            yield connection
            connection.commit()
        except Exception:
            connection.rollback()
            raise
        finally:
            connection.close()

    def initialize(self) -> None:
        with self.transaction(immediate=True) as connection:
            connection.execute(
                "CREATE TABLE IF NOT EXISTS schema_migrations "
                "(version INTEGER PRIMARY KEY, applied_at TEXT NOT NULL)"
            )
            current = connection.execute(
                "SELECT COALESCE(MAX(version), 0) AS version FROM schema_migrations"
            ).fetchone()["version"]
            if current < 1:
                for statement in _MIGRATION_1:
                    connection.execute(statement)
                connection.execute(
                    "INSERT INTO schema_migrations(version, applied_at) VALUES (?, ?)",
                    (1, iso_now()),
                )

    def health(self) -> dict[str, Any]:
        connection = self.connect()
        try:
            row = connection.execute(
                "SELECT COALESCE(MAX(version), 0) AS version FROM schema_migrations"
            ).fetchone()
            return {
                "database": "ok",
                "schema_version": int(row["version"]),
                "journal_mode": connection.execute("PRAGMA journal_mode").fetchone()[0],
            }
        finally:
            connection.close()

    def _one(self, sql: str, params: tuple[Any, ...] = ()) -> dict[str, Any] | None:
        connection = self.connect()
        try:
            row = connection.execute(sql, params).fetchone()
            return dict(row) if row else None
        finally:
            connection.close()

    def _all(self, sql: str, params: tuple[Any, ...] = ()) -> list[dict[str, Any]]:
        connection = self.connect()
        try:
            return [dict(row) for row in connection.execute(sql, params).fetchall()]
        finally:
            connection.close()

    # Authentication and administrator bootstrap.
    def ensure_admin(self, username: str, password_hash: str) -> None:
        now = iso_now()
        with self.transaction(immediate=True) as connection:
            count = connection.execute("SELECT COUNT(*) FROM users").fetchone()[0]
            existing = connection.execute(
                "SELECT id FROM users WHERE username = ?", (username,)
            ).fetchone()
            if existing:
                return
            if count:
                raise DatabaseError("数据库中已有其他管理员账号，未自动新增账号")
            connection.execute(
                "INSERT INTO users(username, password_hash, created_at) VALUES (?, ?, ?)",
                (username, password_hash, now),
            )

    def get_user(self, username: str) -> dict[str, Any] | None:
        return self._one("SELECT id, username, created_at FROM users WHERE username = ?", (username,))

    def get_user_auth(self, username: str) -> dict[str, Any] | None:
        return self._one("SELECT id, username, password_hash FROM users WHERE username = ?", (username,))

    def create_session(
        self,
        token_hash: str,
        csrf_hash: str,
        user_id: int,
        expires_at: str,
        ip_address: str | None,
        user_agent: str | None,
    ) -> None:
        with self.transaction() as connection:
            connection.execute(
                "INSERT INTO sessions(token_hash, csrf_hash, user_id, expires_at, created_at, "
                "last_seen_at, ip_address, user_agent) VALUES (?, ?, ?, ?, ?, ?, ?, ?)",
                (token_hash, csrf_hash, user_id, expires_at, iso_now(), iso_now(), ip_address, user_agent),
            )

    def get_session(self, token_hash: str) -> dict[str, Any] | None:
        row = self._one(
            "SELECT s.token_hash, s.csrf_hash, s.user_id, s.expires_at, s.last_seen_at, "
            "u.username FROM sessions s JOIN users u ON u.id = s.user_id WHERE s.token_hash = ?",
            (token_hash,),
        )
        if row and row["expires_at"] <= iso_now():
            self.delete_session(token_hash)
            return None
        return row

    def touch_session(self, token_hash: str) -> None:
        with self.transaction() as connection:
            connection.execute("UPDATE sessions SET last_seen_at = ? WHERE token_hash = ?", (iso_now(), token_hash))

    def rotate_csrf(self, token_hash: str, csrf_hash: str) -> None:
        with self.transaction() as connection:
            connection.execute("UPDATE sessions SET csrf_hash = ? WHERE token_hash = ?", (csrf_hash, token_hash))

    def verify_csrf(self, token_hash: str, csrf_hash: str) -> bool:
        row = self._one("SELECT csrf_hash FROM sessions WHERE token_hash = ?", (token_hash,))
        return bool(row and row["csrf_hash"] == csrf_hash)

    def delete_session(self, token_hash: str) -> None:
        with self.transaction() as connection:
            connection.execute("DELETE FROM sessions WHERE token_hash = ?", (token_hash,))

    def purge_expired_sessions(self) -> None:
        with self.transaction() as connection:
            connection.execute("DELETE FROM sessions WHERE expires_at <= ?", (iso_now(),))

    # Nodes and enrollment credentials.
    def create_node_with_token(
        self,
        node: dict[str, Any],
        token_hash: str,
        token_expires_at: str,
    ) -> dict[str, Any]:
        with self.transaction(immediate=True) as connection:
            connection.execute(
                "INSERT INTO nodes(id, name, region, tags_json, warp_adapter, xui_unit, notes, "
                "created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)",
                (
                    node["id"], node["name"], node.get("region"), json_dumps(node.get("tags", [])),
                    node["warp_adapter"], node["xui_unit"], node.get("notes"), iso_now(), iso_now(),
                ),
            )
            connection.execute(
                "INSERT INTO enrollment_tokens(id, node_id, token_hash, expires_at, created_at) "
                "VALUES (?, ?, ?, ?, ?)",
                (node["token_id"], node["id"], token_hash, token_expires_at, iso_now()),
            )
        result = self.get_node(node["id"])
        if not result:
            raise DatabaseError("创建节点后无法读取节点")
        return result

    def issue_enrollment_token(self, node_id: str, token_id: str, token_hash: str, expires_at: str) -> None:
        with self.transaction(immediate=True) as connection:
            node = connection.execute(
                "SELECT id FROM nodes WHERE id = ? AND deleted_at IS NULL", (node_id,)
            ).fetchone()
            if not node:
                raise NotFoundError("节点不存在")
            connection.execute(
                "INSERT INTO enrollment_tokens(id, node_id, token_hash, expires_at, created_at) VALUES (?, ?, ?, ?, ?)",
                (token_id, node_id, token_hash, expires_at, iso_now()),
            )

    def get_node(self, node_id: str, include_deleted: bool = False) -> dict[str, Any] | None:
        suffix = "" if include_deleted else " AND n.deleted_at IS NULL"
        row = self._one(
            "SELECT n.* FROM nodes n WHERE n.id = ?" + suffix,
            (node_id,),
        )
        return _decode_node(row)

    def list_nodes(self, search: str | None = None, online: bool | None = None) -> list[dict[str, Any]]:
        clauses = ["deleted_at IS NULL"]
        params: list[Any] = []
        if search:
            clauses.append("(name LIKE ? OR id LIKE ? OR region LIKE ?)")
            pattern = f"%{search}%"
            params.extend([pattern, pattern, pattern])
        if online is not None:
            clauses.append("online = ?")
            params.append(1 if online else 0)
        rows = self._all(
            "SELECT * FROM nodes WHERE " + " AND ".join(clauses) + " ORDER BY name COLLATE NOCASE, id",
            tuple(params),
        )
        return [_decode_node(row) for row in rows]

    def update_node(self, node_id: str, fields: dict[str, Any]) -> dict[str, Any]:
        allowed = {"name", "region", "tags", "warp_adapter", "xui_unit", "notes"}
        updates = []
        params: list[Any] = []
        for key, value in fields.items():
            if key not in allowed:
                continue
            column = "tags_json" if key == "tags" else key
            updates.append(f"{column} = ?")
            params.append(json_dumps(value) if key == "tags" else value)
        if not updates:
            result = self.get_node(node_id)
            if not result:
                raise NotFoundError("节点不存在")
            return result
        updates.append("updated_at = ?")
        params.extend([iso_now(), node_id])
        with self.transaction(immediate=True) as connection:
            cursor = connection.execute(
                "UPDATE nodes SET " + ", ".join(updates) + " WHERE id = ? AND deleted_at IS NULL",
                tuple(params),
            )
            if cursor.rowcount != 1:
                raise NotFoundError("节点不存在")
        result = self.get_node(node_id)
        if not result:
            raise NotFoundError("节点不存在")
        return result

    def soft_delete_node(self, node_id: str) -> None:
        with self.transaction(immediate=True) as connection:
            cursor = connection.execute(
                "UPDATE nodes SET deleted_at = ?, updated_at = ?, online = 0 WHERE id = ? AND deleted_at IS NULL",
                (iso_now(), iso_now(), node_id),
            )
            if cursor.rowcount != 1:
                raise NotFoundError("节点不存在")
            connection.execute(
                "UPDATE agent_credentials SET status = 'revoked', revoked_at = ? "
                "WHERE node_id = ? AND status = 'active'",
                (iso_now(), node_id),
            )

    def get_active_credential_hash(self, node_id: str) -> dict[str, Any] | None:
        return self._one(
            "SELECT c.id, c.credential_hash, n.id AS node_id FROM agent_credentials c "
            "JOIN nodes n ON n.id = c.node_id WHERE c.node_id = ? AND c.status = 'active' "
            "AND n.deleted_at IS NULL ORDER BY c.created_at DESC LIMIT 1",
            (node_id,),
        )

    def rotate_credential(self, node_id: str, credential_id: str, credential_hash: str) -> None:
        with self.transaction(immediate=True) as connection:
            node = connection.execute(
                "SELECT id FROM nodes WHERE id = ? AND deleted_at IS NULL", (node_id,)
            ).fetchone()
            if not node:
                raise NotFoundError("节点不存在")
            now = iso_now()
            connection.execute(
                "UPDATE agent_credentials SET status = 'revoked', revoked_at = ? "
                "WHERE node_id = ? AND status = 'active'",
                (now, node_id),
            )
            connection.execute(
                "INSERT INTO agent_credentials(id, node_id, credential_hash, status, created_at) "
                "VALUES (?, ?, ?, 'active', ?)",
                (credential_id, node_id, credential_hash, now),
            )

    def revoke_credentials(self, node_id: str) -> None:
        with self.transaction(immediate=True) as connection:
            connection.execute(
                "UPDATE agent_credentials SET status = 'revoked', revoked_at = ? "
                "WHERE node_id = ? AND status = 'active'",
                (iso_now(), node_id),
            )

    def enroll_with_token(self, token_hash: str, credential_id: str, credential_hash: str) -> dict[str, Any]:
        now = iso_now()
        with self.transaction(immediate=True) as connection:
            row = connection.execute(
                "SELECT t.id, t.node_id FROM enrollment_tokens t JOIN nodes n ON n.id = t.node_id "
                "WHERE t.token_hash = ? AND t.used_at IS NULL AND t.expires_at > ? AND n.deleted_at IS NULL",
                (token_hash, now),
            ).fetchone()
            if not row:
                raise ConflictError("注册 Token 无效、已使用或已过期")
            connection.execute("UPDATE enrollment_tokens SET used_at = ? WHERE id = ?", (now, row["id"]))
            connection.execute(
                "UPDATE agent_credentials SET status = 'revoked', revoked_at = ? "
                "WHERE node_id = ? AND status = 'active'",
                (now, row["node_id"]),
            )
            connection.execute(
                "INSERT INTO agent_credentials(id, node_id, credential_hash, status, created_at) "
                "VALUES (?, ?, ?, 'active', ?)",
                (credential_id, row["node_id"], credential_hash, now),
            )
        return self.get_node(row["node_id"]) or {}

    def update_node_presence(
        self,
        node_id: str,
        online: bool,
        last_seen_at: str,
        agent_version: str | None = None,
        protocol_version: int | None = None,
        status: dict[str, Any] | None = None,
    ) -> None:
        fields = ["online = ?", "last_seen_at = ?", "updated_at = ?"]
        params: list[Any] = [1 if online else 0, last_seen_at, iso_now()]
        if online:
            fields.append("last_connected_at = COALESCE(last_connected_at, ?)")
            params.append(last_seen_at)
        if agent_version is not None:
            fields.append("agent_version = ?")
            params.append(agent_version)
        if protocol_version is not None:
            fields.append("protocol_version = ?")
            params.append(protocol_version)
        if status is not None:
            fields.extend([
                "status_json = ?", "egress_ipv4 = ?", "egress_ipv6 = ?", "warp_status = ?", "xui_status = ?",
            ])
            params.extend([
                json_dumps(status), status.get("egress_ipv4"), status.get("egress_ipv6"),
                status.get("warp_status", "unknown"), status.get("xui_status", "unknown"),
            ])
        params.append(node_id)
        with self.transaction() as connection:
            connection.execute("UPDATE nodes SET " + ", ".join(fields) + " WHERE id = ?", tuple(params))

    # Action request state.
    def create_action_request(self, request: dict[str, Any]) -> tuple[dict[str, Any], bool]:
        with self.transaction(immediate=True) as connection:
            existing = connection.execute(
                "SELECT * FROM action_requests WHERE request_id = ?", (request["request_id"],)
            ).fetchone()
            if existing:
                return _decode_action(dict(existing)), True
            node = connection.execute(
                "SELECT id FROM nodes WHERE id = ? AND deleted_at IS NULL", (request["node_id"],)
            ).fetchone()
            if not node:
                raise NotFoundError("节点不存在")
            if request.get("mutating"):
                busy = connection.execute(
                    "SELECT request_id FROM action_requests WHERE node_id = ? AND mutating = 1 "
                    "AND status IN ('queued', 'dispatched', 'accepted', 'running') LIMIT 1",
                    (request["node_id"],),
                ).fetchone()
                if busy:
                    raise ConflictError("同一节点已有状态变更动作执行中")
            connection.execute(
                "INSERT INTO action_requests(request_id, node_id, action, parameters_json, source, status, "
                "mutating, created_at, deadline_at, batch_id, task_id, task_run_id, updated_at) "
                "VALUES (?, ?, ?, ?, ?, 'queued', ?, ?, ?, ?, ?, ?, ?)",
                (
                    request["request_id"], request["node_id"], request["action"], json_dumps(request["parameters"]),
                    request["source"], 1 if request.get("mutating") else 0, request["created_at"],
                    request["deadline_at"], request.get("batch_id"), request.get("task_id"),
                    request.get("task_run_id"), request["created_at"],
                ),
            )
            row = connection.execute(
                "SELECT * FROM action_requests WHERE request_id = ?", (request["request_id"],)
            ).fetchone()
            return _decode_action(dict(row)), False

    def get_action(self, request_id: str) -> dict[str, Any] | None:
        return _decode_action(self._one("SELECT * FROM action_requests WHERE request_id = ?", (request_id,)))

    def list_actions(self, node_id: str | None = None, limit: int = 100) -> list[dict[str, Any]]:
        if node_id:
            rows = self._all(
                "SELECT * FROM action_requests WHERE node_id = ? ORDER BY created_at DESC LIMIT ?",
                (node_id, limit),
            )
        else:
            rows = self._all("SELECT * FROM action_requests ORDER BY created_at DESC LIMIT ?", (limit,))
        return [_decode_action(row) for row in rows]

    def set_action_status(
        self,
        request_id: str,
        status: str,
        error_code: str | None = None,
        error_message: str | None = None,
        progress: dict[str, Any] | None = None,
    ) -> dict[str, Any] | None:
        now = iso_now()
        timestamp_column = {
            "dispatched": "dispatched_at", "accepted": "accepted_at", "running": "started_at",
            "succeeded": "finished_at", "failed": "finished_at", "timed_out": "finished_at",
            "unknown": "finished_at", "skipped_offline": "finished_at", "expired": "finished_at",
        }.get(status)
        with self.transaction(immediate=True) as connection:
            row = connection.execute(
                "SELECT * FROM action_requests WHERE request_id = ?", (request_id,)
            ).fetchone()
            if not row:
                return None
            if row["status"] in {"succeeded", "failed", "timed_out", "expired", "skipped_offline", "canceled"}:
                return _decode_action(dict(row))
            updates = ["status = ?", "updated_at = ?"]
            params: list[Any] = [status, now]
            if timestamp_column:
                updates.append(f"{timestamp_column} = COALESCE({timestamp_column}, ?)")
                params.append(now)
            if error_code is not None:
                updates.append("error_code = ?")
                params.append(error_code)
            if error_message is not None:
                updates.append("error_message = ?")
                params.append(error_message[:1000])
            if progress is not None:
                updates.append("progress_json = ?")
                params.append(json_dumps(progress))
            params.append(request_id)
            connection.execute("UPDATE action_requests SET " + ", ".join(updates) + " WHERE request_id = ?", tuple(params))
            fresh = connection.execute("SELECT * FROM action_requests WHERE request_id = ?", (request_id,)).fetchone()
            return _decode_action(dict(fresh))

    def save_action_result(
        self,
        request_id: str,
        status: str,
        result: dict[str, Any] | None,
        error_code: str | None,
        error_message: str | None,
    ) -> dict[str, Any] | None:
        with self.transaction(immediate=True) as connection:
            row = connection.execute(
                "SELECT * FROM action_requests WHERE request_id = ?", (request_id,)
            ).fetchone()
            if not row:
                return None
            if row["status"] not in {"succeeded", "failed"}:
                now = iso_now()
                connection.execute(
                    "UPDATE action_requests SET status = ?, finished_at = ?, updated_at = ?, error_code = ?, "
                    "error_message = ?, result_json = ? WHERE request_id = ?",
                    (status, now, now, error_code, (error_message or "")[:1000] or None, json_dumps(result or {}), request_id),
                )
            connection.execute(
                "INSERT OR REPLACE INTO action_results(request_id, result_json, created_at) VALUES (?, ?, ?)",
                (request_id, json_dumps(result or {}), iso_now()),
            )
            fresh = connection.execute("SELECT * FROM action_requests WHERE request_id = ?", (request_id,)).fetchone()
            return _decode_action(dict(fresh))

    def mark_node_actions_unknown(self, node_id: str) -> int:
        with self.transaction(immediate=True) as connection:
            cursor = connection.execute(
                "UPDATE action_requests SET status = 'unknown', error_code = 'result_unknown', "
                "error_message = 'Agent 连接中断，等待重连对账', finished_at = ?, updated_at = ? "
                "WHERE node_id = ? AND status IN ('queued', 'dispatched', 'accepted', 'running')",
                (iso_now(), iso_now(), node_id),
            )
            return cursor.rowcount

    # Scheduler persistence and lease.
    def try_scheduler_lease(self, owner: str, lease_seconds: int) -> bool:
        now = utc_now()
        lease_until = to_iso(now.replace(microsecond=0) + __import__("datetime").timedelta(seconds=lease_seconds))
        with self.transaction(immediate=True) as connection:
            row = connection.execute("SELECT owner, lease_until FROM scheduler_lock WHERE id = 1").fetchone()
            if row and row["owner"] not in (None, owner) and row["lease_until"] > iso_now():
                return False
            connection.execute(
                "UPDATE scheduler_lock SET owner = ?, lease_until = ? WHERE id = 1",
                (owner, lease_until),
            )
            return True

    def release_scheduler_lease(self, owner: str) -> None:
        with self.transaction(immediate=True) as connection:
            connection.execute(
                "UPDATE scheduler_lock SET owner = NULL, lease_until = NULL WHERE id = ? AND owner = ?",
                (1, owner),
            )

    def create_task(self, task: dict[str, Any]) -> dict[str, Any]:
        with self.transaction(immediate=True) as connection:
            connection.execute(
                "INSERT INTO scheduled_tasks(id, name, node_ids_json, action, parameters_json, schedule_type, "
                "schedule_value, timezone, enabled, max_retries, retry_interval_seconds, catchup_window_seconds, "
                "next_run_at, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
                (
                    task["id"], task["name"], json_dumps(task["node_ids"]), task["action"],
                    json_dumps(task["parameters"]), task["schedule_type"], task["schedule_value"], task["timezone"],
                    1 if task["enabled"] else 0, task["max_retries"], task["retry_interval_seconds"],
                    task["catchup_window_seconds"], task["next_run_at"], iso_now(), iso_now(),
                ),
            )
        return self.get_task(task["id"]) or {}

    def get_task(self, task_id: str) -> dict[str, Any] | None:
        return _decode_task(self._one("SELECT * FROM scheduled_tasks WHERE id = ?", (task_id,)))

    def list_tasks(self) -> list[dict[str, Any]]:
        return [_decode_task(row) for row in self._all("SELECT * FROM scheduled_tasks ORDER BY name COLLATE NOCASE")]

    def update_task(self, task_id: str, fields: dict[str, Any]) -> dict[str, Any]:
        allowed = {
            "name", "node_ids", "action", "parameters", "schedule_type", "schedule_value", "timezone",
            "enabled", "max_retries", "retry_interval_seconds", "catchup_window_seconds", "next_run_at",
        }
        updates = []
        params: list[Any] = []
        for key, value in fields.items():
            if key not in allowed:
                continue
            column = {"node_ids": "node_ids_json", "parameters": "parameters_json"}.get(key, key)
            updates.append(f"{column} = ?")
            params.append(json_dumps(value) if key in {"node_ids", "parameters"} else (1 if value is True else 0 if value is False else value))
        if updates:
            updates.append("updated_at = ?")
            params.extend([iso_now(), task_id])
            with self.transaction(immediate=True) as connection:
                cursor = connection.execute("UPDATE scheduled_tasks SET " + ", ".join(updates) + " WHERE id = ?", tuple(params))
                if cursor.rowcount != 1:
                    raise NotFoundError("任务不存在")
        result = self.get_task(task_id)
        if not result:
            raise NotFoundError("任务不存在")
        return result

    def delete_task(self, task_id: str) -> None:
        with self.transaction(immediate=True) as connection:
            cursor = connection.execute("DELETE FROM scheduled_tasks WHERE id = ?", (task_id,))
            if cursor.rowcount != 1:
                raise NotFoundError("任务不存在")

    def claim_due_tasks(self, now_iso: str) -> list[dict[str, Any]]:
        claimed: list[dict[str, Any]] = []
        with self.transaction(immediate=True) as connection:
            rows = connection.execute(
                "SELECT * FROM scheduled_tasks WHERE enabled = 1 AND next_run_at IS NOT NULL AND next_run_at <= ? "
                "ORDER BY next_run_at LIMIT 25",
                (now_iso,),
            ).fetchall()
            for row in rows:
                task = _decode_task(dict(row))
                scheduled_for = task["next_run_at"]
                run_id = __import__("uuid").uuid4().hex
                connection.execute(
                    "INSERT OR IGNORE INTO task_runs(id, task_id, scheduled_for, status, attempt, created_at) "
                    "VALUES (?, ?, ?, 'running', 1, ?)",
                    (run_id, task["id"], scheduled_for, iso_now()),
                )
                if connection.execute("SELECT changes()").fetchone()[0] != 1:
                    continue
                connection.execute(
                    "UPDATE scheduled_tasks SET next_run_at = NULL, updated_at = ? WHERE id = ?",
                    (iso_now(), task["id"]),
                )
                task["task_run_id"] = run_id
                claimed.append(task)
        return claimed

    def finish_task_run(self, run_id: str, status: str, request_ids: list[str]) -> None:
        with self.transaction() as connection:
            connection.execute(
                "UPDATE task_runs SET status = ?, request_ids_json = ?, finished_at = ? WHERE id = ?",
                (status, json_dumps(request_ids), iso_now(), run_id),
            )

    def audit(self, event: str, details: dict[str, Any], user_id: int | None = None, node_id: str | None = None) -> None:
        with self.transaction() as connection:
            connection.execute(
                "INSERT INTO audit_logs(id, event, user_id, node_id, details_json, created_at) VALUES (?, ?, ?, ?, ?, ?)",
                (__import__("uuid").uuid4().hex, event, user_id, node_id, json_dumps(details), iso_now()),
            )


def _decode_node(row: dict[str, Any] | None) -> dict[str, Any] | None:
    if not row:
        return None
    value = dict(row)
    value["tags"] = json_loads(value.pop("tags_json", "[]"), [])
    value["status"] = json_loads(value.pop("status_json", "{}"), {})
    value["online"] = bool(value.get("online"))
    return value


def _decode_action(row: dict[str, Any] | None) -> dict[str, Any] | None:
    if not row:
        return None
    value = dict(row)
    value["parameters"] = json_loads(value.pop("parameters_json", "{}"), {})
    value["result"] = json_loads(value.pop("result_json", "{}"), {})
    value["progress"] = json_loads(value.pop("progress_json", "{}"), {})
    value["mutating"] = bool(value.get("mutating"))
    return value


def _decode_task(row: dict[str, Any] | None) -> dict[str, Any] | None:
    if not row:
        return None
    value = dict(row)
    value["node_ids"] = json_loads(value.pop("node_ids_json", "[]"), [])
    value["parameters"] = json_loads(value.pop("parameters_json", "{}"), {})
    value["enabled"] = bool(value.get("enabled"))
    return value


_MIGRATION_1 = [
    """
    CREATE TABLE users (
        id INTEGER PRIMARY KEY AUTOINCREMENT,
        username TEXT NOT NULL UNIQUE,
        password_hash TEXT NOT NULL,
        created_at TEXT NOT NULL
    )
    """,
    """
    CREATE TABLE sessions (
        token_hash TEXT PRIMARY KEY,
        csrf_hash TEXT NOT NULL,
        user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
        expires_at TEXT NOT NULL,
        created_at TEXT NOT NULL,
        last_seen_at TEXT NOT NULL,
        ip_address TEXT,
        user_agent TEXT
    )
    """,
    """
    CREATE TABLE nodes (
        id TEXT PRIMARY KEY,
        name TEXT NOT NULL,
        region TEXT,
        tags_json TEXT NOT NULL DEFAULT '[]',
        warp_adapter TEXT NOT NULL,
        xui_unit TEXT NOT NULL,
        notes TEXT,
        online INTEGER NOT NULL DEFAULT 0 CHECK (online IN (0, 1)),
        last_seen_at TEXT,
        last_connected_at TEXT,
        agent_version TEXT,
        protocol_version INTEGER,
        status_json TEXT NOT NULL DEFAULT '{}',
        egress_ipv4 TEXT,
        egress_ipv6 TEXT,
        warp_status TEXT NOT NULL DEFAULT 'unknown',
        xui_status TEXT NOT NULL DEFAULT 'unknown',
        created_at TEXT NOT NULL,
        updated_at TEXT NOT NULL,
        deleted_at TEXT
    )
    """,
    """
    CREATE TABLE agent_credentials (
        id TEXT PRIMARY KEY,
        node_id TEXT NOT NULL REFERENCES nodes(id) ON DELETE RESTRICT,
        credential_hash TEXT NOT NULL,
        status TEXT NOT NULL CHECK (status IN ('active', 'revoked')),
        created_at TEXT NOT NULL,
        revoked_at TEXT
    )
    """,
    """
    CREATE TABLE enrollment_tokens (
        id TEXT PRIMARY KEY,
        node_id TEXT NOT NULL REFERENCES nodes(id) ON DELETE RESTRICT,
        token_hash TEXT NOT NULL UNIQUE,
        expires_at TEXT NOT NULL,
        used_at TEXT,
        created_at TEXT NOT NULL
    )
    """,
    "CREATE INDEX idx_agent_credentials_node_status ON agent_credentials(node_id, status)",
    "CREATE INDEX idx_enrollment_tokens_lookup ON enrollment_tokens(token_hash, used_at, expires_at)",
    """
    CREATE TABLE batches (
        id TEXT PRIMARY KEY,
        action TEXT NOT NULL,
        node_ids_json TEXT NOT NULL,
        status TEXT NOT NULL,
        created_at TEXT NOT NULL,
        finished_at TEXT
    )
    """,
    """
    CREATE TABLE scheduled_tasks (
        id TEXT PRIMARY KEY,
        name TEXT NOT NULL,
        node_ids_json TEXT NOT NULL,
        action TEXT NOT NULL,
        parameters_json TEXT NOT NULL,
        schedule_type TEXT NOT NULL,
        schedule_value TEXT NOT NULL,
        timezone TEXT NOT NULL,
        enabled INTEGER NOT NULL DEFAULT 1 CHECK (enabled IN (0, 1)),
        max_retries INTEGER NOT NULL DEFAULT 2,
        retry_interval_seconds INTEGER NOT NULL DEFAULT 30,
        catchup_window_seconds INTEGER NOT NULL DEFAULT 300,
        next_run_at TEXT,
        created_at TEXT NOT NULL,
        updated_at TEXT NOT NULL
    )
    """,
    """
    CREATE TABLE task_runs (
        id TEXT PRIMARY KEY,
        task_id TEXT NOT NULL REFERENCES scheduled_tasks(id) ON DELETE CASCADE,
        scheduled_for TEXT NOT NULL,
        status TEXT NOT NULL,
        attempt INTEGER NOT NULL DEFAULT 1,
        request_ids_json TEXT NOT NULL DEFAULT '[]',
        created_at TEXT NOT NULL,
        finished_at TEXT,
        UNIQUE(task_id, scheduled_for)
    )
    """,
    """
    CREATE TABLE action_requests (
        request_id TEXT PRIMARY KEY,
        node_id TEXT NOT NULL REFERENCES nodes(id) ON DELETE RESTRICT,
        action TEXT NOT NULL,
        parameters_json TEXT NOT NULL,
        source TEXT NOT NULL,
        status TEXT NOT NULL,
        mutating INTEGER NOT NULL DEFAULT 0 CHECK (mutating IN (0, 1)),
        created_at TEXT NOT NULL,
        dispatched_at TEXT,
        accepted_at TEXT,
        started_at TEXT,
        finished_at TEXT,
        deadline_at TEXT NOT NULL,
        error_code TEXT,
        error_message TEXT,
        progress_json TEXT NOT NULL DEFAULT '{}',
        result_json TEXT NOT NULL DEFAULT '{}',
        batch_id TEXT REFERENCES batches(id) ON DELETE SET NULL,
        task_id TEXT REFERENCES scheduled_tasks(id) ON DELETE SET NULL,
        task_run_id TEXT REFERENCES task_runs(id) ON DELETE SET NULL,
        updated_at TEXT NOT NULL
    )
    """,
    "CREATE INDEX idx_action_requests_node_status ON action_requests(node_id, status, mutating)",
    "CREATE INDEX idx_action_requests_created ON action_requests(created_at)",
    """
    CREATE TABLE action_results (
        request_id TEXT PRIMARY KEY REFERENCES action_requests(request_id) ON DELETE CASCADE,
        result_json TEXT NOT NULL,
        created_at TEXT NOT NULL
    )
    """,
    """
    CREATE TABLE audit_logs (
        id TEXT PRIMARY KEY,
        event TEXT NOT NULL,
        user_id INTEGER REFERENCES users(id) ON DELETE SET NULL,
        node_id TEXT REFERENCES nodes(id) ON DELETE SET NULL,
        details_json TEXT NOT NULL,
        created_at TEXT NOT NULL
    )
    """,
    """
    CREATE TABLE settings (
        key TEXT PRIMARY KEY,
        value_json TEXT NOT NULL,
        updated_at TEXT NOT NULL
    )
    """,
    """
    CREATE TABLE scheduler_lock (
        id INTEGER PRIMARY KEY CHECK (id = 1),
        owner TEXT,
        lease_until TEXT
    )
    """,
    "INSERT INTO scheduler_lock(id, owner, lease_until) VALUES (1, NULL, NULL)",
]

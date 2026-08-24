from __future__ import annotations

import json
import uuid
from datetime import timedelta
from typing import Any, TYPE_CHECKING

from .actions import STATE_CHANGING_ACTIONS, validate_action
from .config import Settings
from .db import Database
from .nodes import NodeService
from .timeutil import iso_now, parse_iso, to_iso, utc_now

if TYPE_CHECKING:
    from .gateway import ConnectionManager


ACTIVE_STATUSES = ("queued", "dispatched", "accepted", "running", "unknown")
TERMINAL_STATUSES = ("succeeded", "failed", "timed_out", "expired", "skipped_offline", "canceled")
LONG_RUNNING_ACTIONS = frozenset({"install_warp", "install_xui", "backup_xui", "restore_xui"})


class RequestConflictError(ValueError):
    def __init__(self, code: str, message: str):
        self.code = code
        self.message = message
        super().__init__(message)


def _json(value: Any) -> str:
    return json.dumps(value, separators=(",", ":"), ensure_ascii=True)


def request_to_dict(row: Any) -> dict[str, Any]:
    result = dict(row)
    result["parameters"] = json.loads(result.pop("parameters_json"))
    raw_result = result.pop("result_json", None)
    result["result"] = json.loads(raw_result) if raw_result else None
    return result


class ActionService:
    def __init__(
        self,
        db: Database,
        settings: Settings,
        nodes: NodeService,
        gateway: "ConnectionManager | None" = None,
    ):
        self.db = db
        self.settings = settings
        self.nodes = nodes
        self.gateway = gateway

    def attach_gateway(self, gateway: "ConnectionManager") -> None:
        self.gateway = gateway

    def get_request(self, request_id: str) -> dict[str, Any] | None:
        row = self.db.fetchone(
            "SELECT * FROM action_requests WHERE id = ?", (request_id,)
        )
        return request_to_dict(row) if row else None

    def list_requests(self, node_id: str | None = None, limit: int = 100) -> list[dict[str, Any]]:
        limit = max(1, min(limit, 500))
        if node_id:
            rows = self.db.fetchall(
                "SELECT * FROM action_requests WHERE node_id = ? ORDER BY created_at DESC LIMIT ?",
                (node_id, limit),
            )
        else:
            rows = self.db.fetchall(
                "SELECT * FROM action_requests ORDER BY created_at DESC LIMIT ?", (limit,)
            )
        return [request_to_dict(row) for row in rows]

    async def create_request(
        self,
        *,
        node_id: str,
        action: str,
        parameters: dict[str, Any],
        source: str,
        actor_id: str | None = None,
        request_id: str | None = None,
        queue_if_offline: bool = False,
        task_id: str | None = None,
        task_run_id: str | None = None,
        batch_id: str | None = None,
    ) -> dict[str, Any]:
        normalized_parameters = validate_action(action, parameters)
        node = self.nodes.get(node_id)
        if not node:
            raise RequestConflictError("node_not_found", "node was not found")
        request_id = request_id or str(uuid.uuid4())
        now = utc_now()
        now_text = to_iso(now)
        ttl_seconds = self.settings.action_ttl_seconds
        if action in LONG_RUNNING_ACTIONS:
            ttl_seconds = max(ttl_seconds, 1800)
        deadline_text = to_iso(now + timedelta(seconds=ttl_seconds))
        online = bool(self.gateway and self.gateway.is_online(node_id))
        requested_status = "queued" if online or queue_if_offline else "skipped_offline"
        error_code = None if requested_status == "queued" else "node_offline"
        error_message = None if requested_status == "queued" else "Agent is offline; request was not queued"

        with self.db.transaction(immediate=True) as connection:
            duplicate = connection.execute(
                "SELECT * FROM action_requests WHERE id = ?", (request_id,)
            ).fetchone()
            if duplicate:
                existing = request_to_dict(duplicate)
                if existing["action"] != action or existing["parameters"] != normalized_parameters:
                    raise RequestConflictError(
                        "request_duplicate",
                        "request_id already exists with different action parameters",
                    )
                return existing
            if action in STATE_CHANGING_ACTIONS:
                changing_placeholders = ", ".join("?" for _ in STATE_CHANGING_ACTIONS)
                active_placeholders = ", ".join("?" for _ in ACTIVE_STATUSES)
                busy = connection.execute(
                    f"""
                    SELECT id, status FROM action_requests
                    WHERE node_id = ? AND action IN ({changing_placeholders})
                      AND status IN ({active_placeholders})
                    ORDER BY created_at DESC LIMIT 1
                    """,
                    (node_id, *STATE_CHANGING_ACTIONS, *ACTIVE_STATUSES),
                ).fetchone()
                if busy:
                    raise RequestConflictError(
                        "action_busy",
                        f"node already has an active state-changing request ({busy['id']})",
                    )
            connection.execute(
                """
                INSERT INTO action_requests
                  (id, node_id, batch_id, task_id, task_run_id, action, parameters_json,
                   source, status, error_code, error_message, attempts, issued_at, deadline_at,
                   created_at, updated_at)
                VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0, ?, ?, ?, ?)
                """,
                (
                    request_id,
                    node_id,
                    batch_id,
                    task_id,
                    task_run_id,
                    action,
                    _json(normalized_parameters),
                    source,
                    requested_status,
                    error_code,
                    error_message,
                    now_text,
                    deadline_text,
                    now_text,
                    now_text,
                ),
            )
            connection.execute(
                """
                INSERT INTO audit_logs
                  (id, actor_type, actor_id, event_type, node_id, metadata_json, created_at)
                VALUES (?, ?, ?, ?, ?, ?, ?)
                """,
                (
                    str(uuid.uuid4()),
                    "admin" if source != "scheduled" else "scheduler",
                    actor_id,
                    "action_requested",
                    node_id,
                    _json({"request_id": request_id, "action": action, "source": source}),
                    now_text,
                ),
            )
        if requested_status == "queued" and online:
            await self.dispatch_request(request_id)
        result = self.get_request(request_id)
        assert result is not None
        return result

    async def dispatch_request(self, request_id: str) -> dict[str, Any] | None:
        row = self.db.fetchone("SELECT * FROM action_requests WHERE id = ?", (request_id,))
        if not row or row["status"] != "queued":
            return request_to_dict(row) if row else None
        deadline = parse_iso(row["deadline_at"])
        if deadline and deadline <= utc_now():
            self._mark_expired(request_id, queued=True)
            return self.get_request(request_id)
        if not self.gateway or not self.gateway.is_online(row["node_id"]):
            return request_to_dict(row)
        now = iso_now()
        with self.db.transaction(immediate=True) as connection:
            current = connection.execute(
                "SELECT * FROM action_requests WHERE id = ?", (request_id,)
            ).fetchone()
            if not current or current["status"] != "queued":
                return request_to_dict(current) if current else None
            connection.execute(
                """
                UPDATE action_requests
                SET status = 'dispatched', attempts = attempts + 1,
                    dispatched_at = COALESCE(dispatched_at, ?), updated_at = ?
                WHERE id = ? AND status = 'queued'
                """,
                (now, now, request_id),
            )
            current = connection.execute(
                "SELECT * FROM action_requests WHERE id = ?", (request_id,)
            ).fetchone()
        try:
            await self.gateway.send_command(current)
        except Exception as exc:
            self.mark_unknown(request_id, "agent_disconnect", "Agent connection dropped while dispatching")
        return self.get_request(request_id)

    async def dispatch_pending(self, node_id: str) -> None:
        rows = self.db.fetchall(
            """
            SELECT id FROM action_requests
            WHERE node_id = ? AND status = 'queued'
            ORDER BY created_at ASC LIMIT 100
            """,
            (node_id,),
        )
        for row in rows:
            await self.dispatch_request(row["id"])

    def mark_unknown(self, request_id: str, error_code: str, message: str) -> None:
        now = iso_now()
        with self.db.transaction(immediate=True) as connection:
            connection.execute(
                """
                UPDATE action_requests
                SET status = 'unknown', error_code = ?, error_message = ?, updated_at = ?
                WHERE id = ? AND status IN ('dispatched', 'accepted', 'running')
                """,
                (error_code, message, now, request_id),
            )

    def mark_unknown_for_node(self, node_id: str) -> None:
        now = iso_now()
        with self.db.transaction(immediate=True) as connection:
            connection.execute(
                """
                UPDATE action_requests
                SET status = 'unknown', error_code = 'result_unknown',
                    error_message = 'Agent connection closed before a final result', updated_at = ?
                WHERE node_id = ? AND status IN ('dispatched', 'accepted', 'running')
                """,
                (now, node_id),
            )

    def reconcile_result(self, node_id: str, payload: dict[str, Any]) -> None:
        row = self.db.fetchone(
            "SELECT node_id FROM action_requests WHERE id = ?", (payload["request_id"],)
        )
        if not row or row["node_id"] != node_id:
            return
        self.finish_request(
            node_id=node_id,
            request_id=payload["request_id"],
            success=payload["success"],
            error_code=payload.get("error_code"),
            error_message=payload.get("error_message"),
            result=payload.get("result") or {},
        )

    def accept_request(self, node_id: str, payload: dict[str, Any]) -> None:
        request_id = payload["request_id"]
        now = iso_now()
        with self.db.transaction(immediate=True) as connection:
            row = connection.execute(
                "SELECT node_id, status FROM action_requests WHERE id = ?", (request_id,)
            ).fetchone()
            if not row or row["node_id"] != node_id:
                return
            if payload["accepted"]:
                connection.execute(
                    """
                    UPDATE action_requests
                    SET status = 'accepted', accepted_at = COALESCE(accepted_at, ?),
                        error_code = NULL, error_message = NULL, updated_at = ?
                    WHERE id = ? AND status IN ('dispatched', 'unknown')
                    """,
                    (now, now, request_id),
                )
            else:
                connection.execute(
                    """
                    UPDATE action_requests
                    SET status = 'failed', error_code = ?, error_message = ?, finished_at = ?, updated_at = ?
                    WHERE id = ? AND status NOT IN ('succeeded', 'failed', 'timed_out', 'expired', 'canceled')
                    """,
                    (
                        payload.get("error_code") or "agent_rejected",
                        payload.get("error_message") or "Agent rejected the action",
                        now,
                        now,
                        request_id,
                    ),
                )

    def progress_request(self, node_id: str, payload: dict[str, Any]) -> None:
        now = iso_now()
        with self.db.transaction(immediate=True) as connection:
            connection.execute(
                """
                UPDATE action_requests
                SET status = 'running', started_at = COALESCE(started_at, ?), updated_at = ?
                WHERE id = ? AND node_id = ? AND status IN ('dispatched', 'accepted', 'unknown')
                """,
                (now, now, payload["request_id"], node_id),
            )

    def finish_request(
        self,
        *,
        node_id: str,
        request_id: str,
        success: bool,
        error_code: str | None,
        error_message: str | None,
        result: dict[str, Any],
    ) -> None:
        now = iso_now()
        status = "succeeded" if success else "failed"
        result_text = _json(result)
        if len(result_text.encode("utf-8")) > 64 * 1024:
            result_text = _json({"error": "result_too_large"})
            success = False
            status = "failed"
            error_code = "result_too_large"
            error_message = "Agent result exceeded the 64 KiB limit"
        with self.db.transaction(immediate=True) as connection:
            row = connection.execute(
                "SELECT * FROM action_requests WHERE id = ?", (request_id,)
            ).fetchone()
            if not row or row["node_id"] != node_id:
                return
            if row["status"] in TERMINAL_STATUSES:
                return
            connection.execute(
                """
                UPDATE action_requests
                SET status = ?, error_code = ?, error_message = ?, result_json = ?,
                    finished_at = ?, updated_at = ?
                WHERE id = ? AND node_id = ?
                """,
                (
                    status,
                    None if success else error_code,
                    None if success else error_message,
                    result_text,
                    now,
                    now,
                    request_id,
                    node_id,
                ),
            )
            connection.execute(
                """
                INSERT INTO action_results
                  (id, request_id, node_id, success, error_code, error_message, result_json, created_at)
                VALUES (?, ?, ?, ?, ?, ?, ?, ?)
                """,
                (
                    str(uuid.uuid4()),
                    request_id,
                    node_id,
                    int(success),
                    None if success else error_code,
                    None if success else error_message,
                    result_text,
                    now,
                ),
            )
        if row["action"] == "change_ip":
            self.nodes.record_ip_change(
                node_id,
                request_id,
                result,
                success,
                error_code,
                error_message,
            )

    def expire_requests(self) -> None:
        now = iso_now()
        with self.db.transaction(immediate=True) as connection:
            connection.execute(
                """
                UPDATE action_requests
                SET status = 'expired', error_code = 'request_expired',
                    error_message = 'Request expired before dispatch', finished_at = ?, updated_at = ?
                WHERE status = 'queued' AND deadline_at <= ?
                """,
                (now, now, now),
            )
            connection.execute(
                """
                UPDATE action_requests
                SET status = 'timed_out', error_code = 'action_timeout',
                    error_message = 'Action exceeded its deadline', finished_at = ?, updated_at = ?
                WHERE status IN ('dispatched', 'accepted', 'running') AND deadline_at <= ?
                """,
                (now, now, now),
            )

    def _mark_expired(self, request_id: str, queued: bool) -> None:
        now = iso_now()
        status = "expired" if queued else "timed_out"
        code = "request_expired" if queued else "action_timeout"
        self.db.execute(
            """
            UPDATE action_requests
            SET status = ?, error_code = ?, error_message = ?, finished_at = ?, updated_at = ?
            WHERE id = ?
            """,
            (status, code, "Request deadline elapsed", now, now, request_id),
        )

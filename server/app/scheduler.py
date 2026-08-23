from __future__ import annotations

import asyncio
import json
import logging
import uuid
from datetime import datetime, timedelta, timezone
from zoneinfo import ZoneInfo, ZoneInfoNotFoundError

try:
    from croniter import croniter as _croniter
except ModuleNotFoundError:  # pragma: no cover - exercised only in minimal installations
    _croniter = None

from .action_service import ActionService, RequestConflictError
from .actions import SCHEDULED_ACTIONS, validate_action
from .config import Settings
from .db import Database
from .nodes import NodeService
from .timeutil import iso_now, parse_iso, to_iso, utc_now


logger = logging.getLogger(__name__)


class ScheduleValidationError(ValueError):
    pass


def _cron_field(value: str, minimum: int, maximum: int) -> tuple[set[int], bool]:
    values: set[int] = set()
    is_any = value == "*"
    for part in value.split(","):
        if not part:
            raise ScheduleValidationError("cron contains an empty field")
        base, _, step_text = part.partition("/")
        try:
            step = int(step_text) if step_text else 1
        except ValueError as exc:
            raise ScheduleValidationError("cron step must be an integer") from exc
        if step < 1:
            raise ScheduleValidationError("cron step must be positive")
        if base == "*":
            start, end = minimum, maximum
        elif "-" in base:
            start_text, end_text = base.split("-", 1)
            try:
                start, end = int(start_text), int(end_text)
            except ValueError as exc:
                raise ScheduleValidationError("cron range must contain integers") from exc
        else:
            try:
                start = end = int(base)
            except ValueError as exc:
                raise ScheduleValidationError("cron field must contain integers") from exc
        if start < minimum or end > maximum or start > end:
            raise ScheduleValidationError("cron field is outside its allowed range")
        values.update(range(start, end + 1, step))
    return values, is_any


def _fallback_cron_next(expression: str, reference: datetime) -> datetime:
    fields = expression.split()
    if len(fields) != 5:
        raise ScheduleValidationError("fallback cron requires five fields")
    minute, minute_any = _cron_field(fields[0], 0, 59)
    hour, hour_any = _cron_field(fields[1], 0, 23)
    day, day_any = _cron_field(fields[2], 1, 31)
    month, month_any = _cron_field(fields[3], 1, 12)
    weekday, weekday_any = _cron_field(fields[4], 0, 6)
    candidate = reference.replace(second=0, microsecond=0) + timedelta(minutes=1)
    for _ in range(366 * 24 * 60 * 2):
        day_matches = candidate.day in day
        weekday_matches = candidate.weekday() in weekday
        day_ok = (day_matches or weekday_matches) if not day_any and not weekday_any else (
            weekday_matches if day_any else day_matches
        )
        if (
            candidate.minute in minute
            and candidate.hour in hour
            and candidate.month in month
            and day_ok
        ):
            return candidate
        candidate += timedelta(minutes=1)
    raise ScheduleValidationError("cron expression has no occurrence within the search window")


def validate_timezone(value: str) -> str:
    try:
        ZoneInfo(value)
    except ZoneInfoNotFoundError as exc:
        raise ScheduleValidationError(f"unknown IANA timezone: {value}") from exc
    return value


def _parse_clock(value: str) -> tuple[int, int]:
    try:
        hour_text, minute_text = value.strip().split(":", 1)
        hour, minute = int(hour_text), int(minute_text)
    except (ValueError, AttributeError):
        raise ScheduleValidationError("schedule time must use HH:MM") from None
    if not 0 <= hour <= 23 or not 0 <= minute <= 59:
        raise ScheduleValidationError("schedule time must be within 00:00 and 23:59")
    return hour, minute


def validate_schedule(schedule_type: str, schedule_value: str, timezone_name: str) -> None:
    validate_timezone(timezone_name)
    if schedule_type == "daily":
        _parse_clock(schedule_value)
    elif schedule_type == "weekly":
        parts = schedule_value.strip().split()
        if len(parts) != 2 or not parts[0].isdigit() or not 0 <= int(parts[0]) <= 6:
            raise ScheduleValidationError("weekly schedule must use '<weekday 0-6> HH:MM'")
        _parse_clock(parts[1])
    elif schedule_type == "cron":
        try:
            if _croniter is not None:
                _croniter(schedule_value)
            else:
                _fallback_cron_next(schedule_value, datetime.now(timezone.utc))
        except (ScheduleValidationError, ValueError, TypeError) as exc:
            raise ScheduleValidationError(f"invalid cron expression: {exc}") from exc
    else:
        raise ScheduleValidationError("schedule_type must be daily, weekly, or cron")


def next_run(schedule_type: str, schedule_value: str, timezone_name: str, after: datetime | None = None) -> str:
    validate_schedule(schedule_type, schedule_value, timezone_name)
    zone = ZoneInfo(timezone_name)
    reference_utc = after or utc_now()
    reference_local = reference_utc.astimezone(zone)
    if schedule_type == "cron":
        if _croniter is not None:
            candidate = _croniter(schedule_value, reference_local).get_next(datetime)
        else:
            candidate = _fallback_cron_next(schedule_value, reference_local)
        if candidate.tzinfo is None:
            candidate = candidate.replace(tzinfo=zone)
        return to_iso(candidate.astimezone(timezone.utc))

    if schedule_type == "daily":
        weekdays = None
        clock_value = schedule_value
    else:
        weekday_text, clock_value = schedule_value.strip().split()
        weekdays = {int(weekday_text)}
    hour, minute = _parse_clock(clock_value)
    for day_offset in range(0, 8):
        candidate_date = reference_local.date() + timedelta(days=day_offset)
        if weekdays is not None and candidate_date.weekday() not in weekdays:
            continue
        candidate = datetime(
            candidate_date.year,
            candidate_date.month,
            candidate_date.day,
            hour,
            minute,
            tzinfo=zone,
        )
        if candidate > reference_local:
            return to_iso(candidate.astimezone(timezone.utc))
    raise ScheduleValidationError("could not calculate next schedule time")


def _json(value: object) -> str:
    return json.dumps(value, separators=(",", ":"), ensure_ascii=True)


class TaskService:
    def __init__(self, db: Database, nodes: NodeService, actions: ActionService):
        self.db = db
        self.nodes = nodes
        self.actions = actions

    def list_tasks(self) -> list[dict[str, object]]:
        rows = self.db.fetchall(
            "SELECT * FROM tasks WHERE deleted_at IS NULL ORDER BY created_at DESC"
        )
        return [self._task_dict(row) for row in rows]

    def get_task(self, task_id: str) -> dict[str, object] | None:
        row = self.db.fetchone("SELECT * FROM tasks WHERE id = ? AND deleted_at IS NULL", (task_id,))
        return self._task_dict(row) if row else None

    def list_runs(self, task_id: str, limit: int = 100) -> list[dict[str, object]]:
        rows = self.db.fetchall(
            "SELECT * FROM task_runs WHERE task_id = ? ORDER BY created_at DESC LIMIT ?",
            (task_id, max(1, min(limit, 500))),
        )
        values = []
        for row in rows:
            value = dict(row)
            value["summary"] = json.loads(value.pop("summary_json"))
            values.append(value)
        return values

    def create_task(
        self,
        *,
        name: str,
        node_ids: list[str],
        action: str,
        parameters: dict[str, object],
        schedule_type: str,
        schedule_value: str,
        timezone_name: str,
        enabled: bool,
        max_retries: int,
        retry_intervals_seconds: list[int],
        actor_id: str | None,
    ) -> dict[str, object]:
        if action not in SCHEDULED_ACTIONS:
            raise ScheduleValidationError("scheduled tasks only support state-changing actions")
        normalized_parameters = validate_action(action, parameters)
        if len(set(node_ids)) != len(node_ids):
            raise ScheduleValidationError("node_ids must not contain duplicates")
        missing = [node_id for node_id in node_ids if not self.nodes.get(node_id)]
        if missing:
            raise ScheduleValidationError(f"unknown node_ids: {', '.join(missing)}")
        validate_schedule(schedule_type, schedule_value, timezone_name)
        if any(value < 1 or value > 86400 for value in retry_intervals_seconds):
            raise ScheduleValidationError("retry intervals must be between 1 and 86400 seconds")
        now = utc_now()
        next_at = next_run(schedule_type, schedule_value, timezone_name, now) if enabled else None
        task_id = str(uuid.uuid4())
        now_text = to_iso(now)
        with self.db.transaction(immediate=True) as connection:
            connection.execute(
                """
                INSERT INTO tasks
                  (id, name, node_ids_json, action, parameters_json, schedule_type, schedule_value,
                   timezone, enabled, max_retries, retry_intervals_json, next_run_at, created_at, updated_at)
                VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
                """,
                (
                    task_id,
                    name,
                    _json(node_ids),
                    action,
                    _json(normalized_parameters),
                    schedule_type,
                    schedule_value,
                    timezone_name,
                    int(enabled),
                    max_retries,
                    _json(retry_intervals_seconds),
                    next_at,
                    now_text,
                    now_text,
                ),
            )
            connection.execute(
                """
                INSERT INTO audit_logs
                  (id, actor_type, actor_id, event_type, metadata_json, created_at)
                VALUES (?, 'admin', ?, 'task_created', ?, ?)
                """,
                (str(uuid.uuid4()), actor_id, _json({"task_id": task_id, "action": action}), now_text),
            )
        task = self.get_task(task_id)
        assert task is not None
        return task

    def update_task(self, task_id: str, changes: dict[str, object], actor_id: str | None) -> dict[str, object] | None:
        existing = self.get_task(task_id)
        if not existing:
            return None
        schedule_type = str(changes.get("schedule_type", existing["schedule_type"]))
        schedule_value = str(changes.get("schedule_value", existing["schedule_value"]))
        timezone_name = str(changes.get("timezone", existing["timezone"]))
        validate_schedule(schedule_type, schedule_value, timezone_name)
        allowed = {"name", "enabled", "schedule_type", "schedule_value", "timezone", "max_retries", "retry_intervals_seconds"}
        if set(changes) - allowed:
            raise ScheduleValidationError("unsupported task fields")
        if "retry_intervals_seconds" in changes:
            values = changes["retry_intervals_seconds"]
            if not isinstance(values, list) or any(not isinstance(value, int) or value < 1 or value > 86400 for value in values):
                raise ScheduleValidationError("retry intervals must be between 1 and 86400 seconds")
        columns: list[str] = []
        params: list[object] = []
        for key, value in changes.items():
            column = "retry_intervals_json" if key == "retry_intervals_seconds" else key
            columns.append(f"{column} = ?")
            params.append(_json(value) if key == "retry_intervals_seconds" else int(value) if key == "enabled" else value)
        if "enabled" in changes and changes["enabled"]:
            columns.append("next_run_at = ?")
            params.append(next_run(schedule_type, schedule_value, timezone_name))
        elif "enabled" in changes and not changes["enabled"]:
            columns.append("next_run_at = NULL")
        elif any(key in changes for key in ("schedule_type", "schedule_value", "timezone")) and existing["enabled"]:
            columns.append("next_run_at = ?")
            params.append(next_run(schedule_type, schedule_value, timezone_name))
        if not columns:
            return existing
        now = iso_now()
        columns.append("updated_at = ?")
        params.extend([now, task_id])
        with self.db.transaction(immediate=True) as connection:
            connection.execute(
                f"UPDATE tasks SET {', '.join(columns)} WHERE id = ? AND deleted_at IS NULL",
                tuple(params),
            )
            connection.execute(
                """
                INSERT INTO audit_logs
                  (id, actor_type, actor_id, event_type, metadata_json, created_at)
                VALUES (?, 'admin', ?, 'task_updated', ?, ?)
                """,
                (str(uuid.uuid4()), actor_id, _json({"task_id": task_id, "fields": sorted(changes)}), now),
            )
        return self.get_task(task_id)

    def delete_task(self, task_id: str, actor_id: str | None) -> bool:
        now = iso_now()
        with self.db.transaction(immediate=True) as connection:
            cursor = connection.execute(
                "UPDATE tasks SET enabled = 0, deleted_at = ?, updated_at = ? WHERE id = ? AND deleted_at IS NULL",
                (now, now, task_id),
            )
            if cursor.rowcount == 0:
                return False
            connection.execute(
                """
                INSERT INTO audit_logs
                  (id, actor_type, actor_id, event_type, metadata_json, created_at)
                VALUES (?, 'admin', ?, 'task_deleted', ?, ?)
                """,
                (str(uuid.uuid4()), actor_id, _json({"task_id": task_id}), now),
            )
        return True

    async def claim_due_tasks(self) -> None:
        now = utc_now()
        due_rows = self.db.fetchall(
            "SELECT * FROM tasks WHERE enabled = 1 AND deleted_at IS NULL AND next_run_at IS NOT NULL AND next_run_at <= ? ORDER BY next_run_at LIMIT 50",
            (to_iso(now),),
        )
        for row in due_rows:
            scheduled_for = row["next_run_at"]
            next_at = next_run(row["schedule_type"], row["schedule_value"], row["timezone"], parse_iso(scheduled_for) or now)
            run_id = str(uuid.uuid4())
            now_text = to_iso(now)
            claimed = False
            with self.db.transaction(immediate=True) as connection:
                current = connection.execute(
                    "SELECT next_run_at, enabled FROM tasks WHERE id = ? AND deleted_at IS NULL", (row["id"],)
                ).fetchone()
                if current and current["enabled"] and current["next_run_at"] == scheduled_for:
                    connection.execute(
                        """
                        INSERT OR IGNORE INTO task_runs
                          (id, task_id, scheduled_for, status, created_at)
                        VALUES (?, ?, ?, 'running', ?)
                        """,
                        (run_id, row["id"], scheduled_for, now_text),
                    )
                    claimed = connection.execute("SELECT changes()").fetchone()[0] == 1
                    connection.execute(
                        "UPDATE tasks SET next_run_at = ?, updated_at = ? WHERE id = ?",
                        (next_at, now_text, row["id"]),
                    )
            if claimed:
                await self._start_task_run(row, run_id)

    async def _start_task_run(self, row: object, run_id: str) -> None:
        node_ids = json.loads(row["node_ids_json"])
        parameters = json.loads(row["parameters_json"])
        request_ids: list[str] = []
        counts: dict[str, int] = {}
        for node_id in node_ids:
            try:
                request = await self.actions.create_request(
                    node_id=node_id,
                    action=row["action"],
                    parameters=parameters,
                    source="scheduled",
                    queue_if_offline=False,
                    task_id=row["id"],
                    task_run_id=run_id,
                )
                request_ids.append(request["id"])
                counts[request["status"]] = counts.get(request["status"], 0) + 1
            except RequestConflictError as exc:
                counts[exc.code] = counts.get(exc.code, 0) + 1
        status = "dispatched" if any(key in counts for key in ("queued", "dispatched", "accepted", "running")) else "completed"
        summary = {"request_ids": request_ids, "counts": counts}
        now = iso_now()
        self.db.execute(
            "UPDATE task_runs SET status = ?, summary_json = ?, started_at = COALESCE(started_at, ?), finished_at = ? WHERE id = ?",
            (status, _json(summary), now, now if status == "completed" else None, run_id),
        )

    @staticmethod
    def _task_dict(row: object) -> dict[str, object]:
        value = dict(row)
        value["node_ids"] = json.loads(value.pop("node_ids_json"))
        value["parameters"] = json.loads(value.pop("parameters_json"))
        value["retry_intervals_seconds"] = json.loads(value.pop("retry_intervals_json"))
        value["enabled"] = bool(value["enabled"])
        value.pop("deleted_at", None)
        return value


class Scheduler:
    def __init__(self, db: Database, task_service: TaskService, settings: Settings):
        self.db = db
        self.task_service = task_service
        self.settings = settings
        self.owner_id = str(uuid.uuid4())
        self._stop = asyncio.Event()
        self._task: asyncio.Task[None] | None = None

    async def start(self) -> None:
        if self._task and not self._task.done():
            return
        self._stop.clear()
        self._task = asyncio.create_task(self._run(), name="vps-tool-scheduler")

    async def stop(self) -> None:
        self._stop.set()
        if self._task:
            await self._task
            self._task = None

    def _acquire_lease(self) -> bool:
        now = utc_now()
        until = now + timedelta(seconds=max(10, self.settings.scheduler_interval_seconds * 3))
        now_text = to_iso(now)
        until_text = to_iso(until)
        with self.db.transaction(immediate=True) as connection:
            row = connection.execute(
                "SELECT owner_id, lease_until FROM scheduler_leases WHERE name = 'default'"
            ).fetchone()
            if row and row["owner_id"] != self.owner_id:
                lease_until = parse_iso(row["lease_until"])
                if lease_until and lease_until > now:
                    return False
            connection.execute(
                """
                INSERT INTO scheduler_leases (name, owner_id, lease_until, updated_at)
                VALUES ('default', ?, ?, ?)
                ON CONFLICT(name) DO UPDATE SET owner_id = excluded.owner_id,
                  lease_until = excluded.lease_until, updated_at = excluded.updated_at
                """,
                (self.owner_id, until_text, now_text),
            )
            return True

    async def _run(self) -> None:
        while not self._stop.is_set():
            try:
                if self._acquire_lease():
                    await self.task_service.claim_due_tasks()
                    self.task_service.actions.expire_requests()
            except Exception:
                logger.exception("scheduler iteration failed")
            try:
                await asyncio.wait_for(self._stop.wait(), timeout=self.settings.scheduler_interval_seconds)
            except asyncio.TimeoutError:
                pass

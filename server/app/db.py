from __future__ import annotations

import json
import sqlite3
from contextlib import contextmanager
from datetime import datetime, timezone
from pathlib import Path
from typing import Any, Iterator


SCHEMA_VERSION = 2


SCHEMA_MIGRATIONS: dict[int, str] = {
    1: """
    CREATE TABLE IF NOT EXISTS users (
        id TEXT PRIMARY KEY,
        username TEXT NOT NULL UNIQUE,
        password_hash TEXT NOT NULL,
        created_at TEXT NOT NULL,
        updated_at TEXT NOT NULL
    );

    CREATE TABLE IF NOT EXISTS sessions (
        id TEXT PRIMARY KEY,
        user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
        session_hash TEXT NOT NULL UNIQUE,
        csrf_hash TEXT NOT NULL,
        created_at TEXT NOT NULL,
        expires_at TEXT NOT NULL,
        last_seen_at TEXT NOT NULL
    );
    CREATE INDEX IF NOT EXISTS idx_sessions_expiry ON sessions(expires_at);

    CREATE TABLE IF NOT EXISTS nodes (
        id TEXT PRIMARY KEY,
        name TEXT NOT NULL,
        region TEXT NOT NULL DEFAULT '',
        tags_json TEXT NOT NULL DEFAULT '[]',
        warp_adapter TEXT NOT NULL DEFAULT 'generic',
        xui_service TEXT NOT NULL DEFAULT 'x-ui',
        notes TEXT NOT NULL DEFAULT '',
        agent_version TEXT,
        hostname TEXT,
        os_name TEXT,
        os_version TEXT,
        architecture TEXT,
        cpu_percent REAL,
        memory_used_bytes INTEGER,
        memory_total_bytes INTEGER,
        root_used_bytes INTEGER,
        root_total_bytes INTEGER,
        uptime_seconds INTEGER,
        warp_status TEXT NOT NULL DEFAULT 'unknown',
        xui_status TEXT NOT NULL DEFAULT 'unknown',
        public_ipv4 TEXT,
        public_ipv6 TEXT,
        status_json TEXT NOT NULL DEFAULT '{}',
        last_status_at TEXT,
        last_seen_at TEXT,
        last_ip_change_at TEXT,
        last_ip_change_result_json TEXT,
        created_at TEXT NOT NULL,
        updated_at TEXT NOT NULL,
        deleted_at TEXT
    );
    CREATE INDEX IF NOT EXISTS idx_nodes_deleted ON nodes(deleted_at);
    CREATE INDEX IF NOT EXISTS idx_nodes_last_seen ON nodes(last_seen_at);

    CREATE TABLE IF NOT EXISTS agent_credentials (
        id TEXT PRIMARY KEY,
        node_id TEXT NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
        credential_hash TEXT NOT NULL UNIQUE,
        status TEXT NOT NULL CHECK(status IN ('active', 'revoked')),
        created_at TEXT NOT NULL,
        revoked_at TEXT
    );
    CREATE INDEX IF NOT EXISTS idx_agent_credentials_node ON agent_credentials(node_id, status);

    CREATE TABLE IF NOT EXISTS enrollment_tokens (
        id TEXT PRIMARY KEY,
        node_id TEXT NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
        token_hash TEXT NOT NULL UNIQUE,
        created_at TEXT NOT NULL,
        expires_at TEXT NOT NULL,
        used_at TEXT
    );
    CREATE INDEX IF NOT EXISTS idx_enrollment_tokens_node ON enrollment_tokens(node_id, expires_at);

    CREATE TABLE IF NOT EXISTS batches (
        id TEXT PRIMARY KEY,
        action TEXT NOT NULL,
        created_at TEXT NOT NULL,
        created_by TEXT REFERENCES users(id),
        status TEXT NOT NULL DEFAULT 'queued'
    );

    CREATE TABLE IF NOT EXISTS action_requests (
        id TEXT PRIMARY KEY,
        node_id TEXT NOT NULL REFERENCES nodes(id),
        batch_id TEXT REFERENCES batches(id),
        task_id TEXT,
        task_run_id TEXT,
        action TEXT NOT NULL,
        parameters_json TEXT NOT NULL,
        source TEXT NOT NULL CHECK(source IN ('manual', 'batch', 'scheduled')),
        status TEXT NOT NULL,
        error_code TEXT,
        error_message TEXT,
        result_json TEXT,
        attempts INTEGER NOT NULL DEFAULT 0,
        issued_at TEXT NOT NULL,
        deadline_at TEXT NOT NULL,
        dispatched_at TEXT,
        accepted_at TEXT,
        started_at TEXT,
        finished_at TEXT,
        created_at TEXT NOT NULL,
        updated_at TEXT NOT NULL
    );
    CREATE INDEX IF NOT EXISTS idx_action_requests_node_status ON action_requests(node_id, status);
    CREATE INDEX IF NOT EXISTS idx_action_requests_status_deadline ON action_requests(status, deadline_at);
    CREATE INDEX IF NOT EXISTS idx_action_requests_created ON action_requests(created_at);

    CREATE TABLE IF NOT EXISTS action_results (
        id TEXT PRIMARY KEY,
        request_id TEXT NOT NULL REFERENCES action_requests(id) ON DELETE CASCADE,
        node_id TEXT NOT NULL REFERENCES nodes(id),
        success INTEGER NOT NULL,
        error_code TEXT,
        error_message TEXT,
        result_json TEXT NOT NULL DEFAULT '{}',
        created_at TEXT NOT NULL
    );
    CREATE INDEX IF NOT EXISTS idx_action_results_request ON action_results(request_id, created_at);

    CREATE TABLE IF NOT EXISTS tasks (
        id TEXT PRIMARY KEY,
        name TEXT NOT NULL,
        node_ids_json TEXT NOT NULL,
        action TEXT NOT NULL,
        parameters_json TEXT NOT NULL,
        schedule_type TEXT NOT NULL CHECK(schedule_type IN ('daily', 'weekly', 'cron')),
        schedule_value TEXT NOT NULL,
        timezone TEXT NOT NULL,
        enabled INTEGER NOT NULL DEFAULT 1,
        max_retries INTEGER NOT NULL DEFAULT 2,
        retry_intervals_json TEXT NOT NULL DEFAULT '[30, 90]',
        next_run_at TEXT,
        created_at TEXT NOT NULL,
        updated_at TEXT NOT NULL,
        deleted_at TEXT
    );
    CREATE INDEX IF NOT EXISTS idx_tasks_due ON tasks(enabled, next_run_at);

    CREATE TABLE IF NOT EXISTS task_runs (
        id TEXT PRIMARY KEY,
        task_id TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
        scheduled_for TEXT NOT NULL,
        status TEXT NOT NULL,
        attempt INTEGER NOT NULL DEFAULT 0,
        summary_json TEXT NOT NULL DEFAULT '{}',
        created_at TEXT NOT NULL,
        started_at TEXT,
        finished_at TEXT,
        UNIQUE(task_id, scheduled_for)
    );
    CREATE INDEX IF NOT EXISTS idx_task_runs_task ON task_runs(task_id, created_at);

    CREATE TABLE IF NOT EXISTS audit_logs (
        id TEXT PRIMARY KEY,
        actor_type TEXT NOT NULL,
        actor_id TEXT,
        event_type TEXT NOT NULL,
        node_id TEXT,
        metadata_json TEXT NOT NULL DEFAULT '{}',
        created_at TEXT NOT NULL
    );
    CREATE INDEX IF NOT EXISTS idx_audit_logs_created ON audit_logs(created_at);

    CREATE TABLE IF NOT EXISTS settings (
        key TEXT PRIMARY KEY,
        value_json TEXT NOT NULL,
        updated_at TEXT NOT NULL
    );
    """,
    2: """
    CREATE TABLE IF NOT EXISTS scheduler_leases (
        name TEXT PRIMARY KEY,
        owner_id TEXT NOT NULL,
        lease_until TEXT NOT NULL,
        updated_at TEXT NOT NULL
    );
    CREATE INDEX IF NOT EXISTS idx_scheduler_leases_until ON scheduler_leases(lease_until);
    """,
}


class Database:
    def __init__(self, path: Path):
        self.path = path

    def connect(self) -> sqlite3.Connection:
        self.path.parent.mkdir(parents=True, exist_ok=True)
        connection = sqlite3.connect(
            self.path,
            timeout=5.0,
            isolation_level=None,
            check_same_thread=False,
        )
        connection.row_factory = sqlite3.Row
        connection.execute("PRAGMA foreign_keys = ON")
        connection.execute("PRAGMA busy_timeout = 5000")
        connection.execute("PRAGMA journal_mode = WAL")
        connection.execute("PRAGMA synchronous = NORMAL")
        return connection

    def initialize(self) -> None:
        connection = self.connect()
        try:
            current = int(connection.execute("PRAGMA user_version").fetchone()[0])
            if current > SCHEMA_VERSION:
                raise RuntimeError(
                    f"database schema {current} is newer than supported schema {SCHEMA_VERSION}"
                )
            for version in range(current + 1, SCHEMA_VERSION + 1):
                connection.execute("BEGIN IMMEDIATE")
                try:
                    connection.executescript(SCHEMA_MIGRATIONS[version])
                    connection.execute(f"PRAGMA user_version = {version}")
                    connection.commit()
                except Exception:
                    connection.rollback()
                    raise
        finally:
            connection.close()

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

    def fetchone(self, query: str, params: tuple[Any, ...] = ()) -> sqlite3.Row | None:
        connection = self.connect()
        try:
            return connection.execute(query, params).fetchone()
        finally:
            connection.close()

    def fetchall(self, query: str, params: tuple[Any, ...] = ()) -> list[sqlite3.Row]:
        connection = self.connect()
        try:
            return connection.execute(query, params).fetchall()
        finally:
            connection.close()

    def execute(self, query: str, params: tuple[Any, ...] = ()) -> int:
        with self.transaction(immediate=True) as connection:
            cursor = connection.execute(query, params)
            return cursor.rowcount

    def rotate_csrf(self, session_hash: str, csrf_hash: str) -> None:
        self.execute(
            "UPDATE sessions SET csrf_hash = ?, last_seen_at = ? WHERE session_hash = ?",
            (csrf_hash, datetime.now(timezone.utc).isoformat(), session_hash),
        )

    def insert_audit(
        self,
        *,
        audit_id: str,
        actor_type: str,
        actor_id: str | None,
        event_type: str,
        node_id: str | None,
        metadata: dict[str, Any],
        created_at: str,
    ) -> None:
        self.execute(
            """
            INSERT INTO audit_logs
              (id, actor_type, actor_id, event_type, node_id, metadata_json, created_at)
            VALUES (?, ?, ?, ?, ?, ?, ?)
            """,
            (
                audit_id,
                actor_type,
                actor_id,
                event_type,
                node_id,
                json.dumps(metadata, separators=(",", ":"), ensure_ascii=True),
                created_at,
            ),
        )

from __future__ import annotations

import argparse
import hashlib
import json
import sqlite3
import uuid
from datetime import datetime, timedelta, timezone
from pathlib import Path


NODE_IDS = {
    "tokyo": "00000000-0000-4000-8000-000000000001",
    "frankfurt": "00000000-0000-4000-8000-000000000002",
    "singapore": "00000000-0000-4000-8000-000000000003",
}

AGENTS = [
    {
        "node_id": NODE_IDS["tokyo"],
        "credential": "demo-credential-tokyo-001",
        "hostname": "tokyo-edge-01",
        "architecture": "amd64",
        "agent_version": "0.2.0-demo",
        "warp_status": "on",
        "xui_status": "running",
        "public_ipv4": "203.0.113.21",
        "public_ipv6": "2001:db8::21",
        "cpu_percent": 31.4,
        "memory_used_bytes": 681 * 1024 * 1024,
        "memory_total_bytes": 2048 * 1024 * 1024,
        "root_used_bytes": 12 * 1024 * 1024 * 1024,
        "root_total_bytes": 40 * 1024 * 1024 * 1024,
        "uptime_seconds": 7 * 24 * 60 * 60 + 4 * 60 * 60,
    },
    {
        "node_id": NODE_IDS["singapore"],
        "credential": "demo-credential-singapore-002",
        "hostname": "singapore-egress-01",
        "architecture": "arm64",
        "agent_version": "0.2.0-demo",
        "warp_status": "off",
        "xui_status": "running",
        "public_ipv4": "198.51.100.32",
        "public_ipv6": "2001:db8::32",
        "cpu_percent": 68.2,
        "memory_used_bytes": 1420 * 1024 * 1024,
        "memory_total_bytes": 4096 * 1024 * 1024,
        "root_used_bytes": 29 * 1024 * 1024 * 1024,
        "root_total_bytes": 80 * 1024 * 1024 * 1024,
        "uptime_seconds": 19 * 24 * 60 * 60 + 9 * 60 * 60,
    },
]

NODE_FIXTURES = [
    {
        "id": NODE_IDS["tokyo"],
        "name": "Tokyo Edge 01",
        "region": "Tokyo / JP",
        "tags": ["warp", "primary"],
        "warp_adapter": "warp-cli",
        "xui_service": "x-ui",
        "notes": "Demo online node with a healthy WARP tunnel.",
        "agent": AGENTS[0],
    },
    {
        "id": NODE_IDS["frankfurt"],
        "name": "Frankfurt Relay 02",
        "region": "Frankfurt / DE",
        "tags": ["relay", "attention"],
        "warp_adapter": "wgcf",
        "xui_service": "x-ui",
        "notes": "Demo offline node for the disconnected state.",
        "agent": None,
    },
    {
        "id": NODE_IDS["singapore"],
        "name": "Singapore Egress 03",
        "region": "Singapore / SG",
        "tags": ["arm64", "egress"],
        "warp_adapter": "generic",
        "xui_service": "x-ui",
        "notes": "Demo online ARM node with WARP disabled.",
        "agent": AGENTS[1],
    },
]


def iso(value: datetime) -> str:
    return value.astimezone(timezone.utc).isoformat(timespec="seconds").replace("+00:00", "Z")


def hash_token(value: str) -> str:
    return hashlib.sha256(value.encode("utf-8")).hexdigest()


def status_for(node: dict[str, object], now: datetime) -> dict[str, object]:
    agent = node["agent"]
    if agent is None:
        return {
            "agent_version": "0.2.0-demo",
            "hostname": "frankfurt-relay-02",
            "os_name": "Debian",
            "os_version": "12",
            "architecture": "amd64",
            "cpu_percent": 4.7,
            "memory_used_bytes": 430 * 1024 * 1024,
            "memory_total_bytes": 2048 * 1024 * 1024,
            "root_used_bytes": 18 * 1024 * 1024 * 1024,
            "root_total_bytes": 40 * 1024 * 1024 * 1024,
            "uptime_seconds": 2 * 24 * 60 * 60 + 11 * 60 * 60,
            "warp_status": "unknown",
            "xui_status": "not_found",
            "public_ipv4": "198.51.100.44",
            "public_ipv6": "2001:db8::44",
            "observed_at": iso(now - timedelta(hours=8)),
        }
    return {
        **{key: agent[key] for key in (
            "agent_version",
            "hostname",
            "architecture",
            "warp_status",
            "xui_status",
            "public_ipv4",
            "public_ipv6",
            "cpu_percent",
            "memory_used_bytes",
            "memory_total_bytes",
            "root_used_bytes",
            "root_total_bytes",
            "uptime_seconds",
        )},
        "os_name": "Debian",
        "os_version": "12",
        "observed_at": iso(now),
    }


def insert_demo(db_path: Path, fixture_path: Path) -> None:
    now = datetime.now(timezone.utc)
    now_text = iso(now)
    old_seen = iso(now - timedelta(hours=8))
    future = iso(now + timedelta(hours=4))

    connection = sqlite3.connect(db_path)
    connection.execute("PRAGMA foreign_keys = ON")
    try:
        with connection:
            for table in (
                "action_results",
                "action_requests",
                "task_runs",
                "tasks",
                "agent_credentials",
                "enrollment_tokens",
                "audit_logs",
                "nodes",
            ):
                connection.execute(f"DELETE FROM {table}")

            for node in NODE_FIXTURES:
                status = status_for(node, now)
                connection.execute(
                    """
                    INSERT INTO nodes
                      (id, name, region, tags_json, warp_adapter, xui_service, notes,
                       agent_version, hostname, os_name, os_version, architecture,
                       cpu_percent, memory_used_bytes, memory_total_bytes,
                       root_used_bytes, root_total_bytes, uptime_seconds,
                       warp_status, xui_status, public_ipv4, public_ipv6, status_json,
                       last_status_at, last_seen_at, created_at, updated_at)
                    VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
                    """,
                    (
                        node["id"],
                        node["name"],
                        node["region"],
                        json.dumps(node["tags"], separators=(",", ":")),
                        node["warp_adapter"],
                        node["xui_service"],
                        node["notes"],
                        status["agent_version"],
                        status["hostname"],
                        status["os_name"],
                        status["os_version"],
                        status["architecture"],
                        status["cpu_percent"],
                        status["memory_used_bytes"],
                        status["memory_total_bytes"],
                        status["root_used_bytes"],
                        status["root_total_bytes"],
                        status["uptime_seconds"],
                        status["warp_status"],
                        status["xui_status"],
                        status["public_ipv4"],
                        status["public_ipv6"],
                        json.dumps(status, separators=(",", ":")),
                        status["observed_at"],
                        now_text if node["agent"] else old_seen,
                        iso(now - timedelta(days=14)),
                        now_text,
                    ),
                )

            for agent in AGENTS:
                connection.execute(
                    """
                    INSERT INTO agent_credentials (id, node_id, credential_hash, status, created_at)
                    VALUES (?, ?, ?, 'active', ?)
                    """,
                    (str(uuid.uuid4()), agent["node_id"], hash_token(agent["credential"]), now_text),
                )

            task_id = "10000000-0000-4000-8000-000000000001"
            connection.execute(
                """
                INSERT INTO tasks
                  (id, name, node_ids_json, action, parameters_json, schedule_type,
                   schedule_value, timezone, enabled, max_retries, retry_intervals_json,
                   next_run_at, created_at, updated_at)
                VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
                """,
                (
                    task_id,
                    "Nightly WARP rotation",
                    json.dumps([NODE_IDS["tokyo"], NODE_IDS["singapore"]], separators=(",", ":")),
                    "change_ip",
                    json.dumps({"max_attempts": 3, "timeout_seconds": 180}, separators=(",", ":")),
                    "daily",
                    "03:30",
                    "Asia/Shanghai",
                    1,
                    2,
                    json.dumps([30, 90], separators=(",", ":")),
                    future,
                    iso(now - timedelta(days=3)),
                    now_text,
                ),
            )

            requests = [
                {
                    "id": "20000000-0000-4000-8000-000000000001",
                    "node_id": NODE_IDS["tokyo"],
                    "action": "change_ip",
                    "parameters": {"max_attempts": 3, "timeout_seconds": 180},
                    "status": "succeeded",
                    "source": "manual",
                    "result": {"before_ipv4": "198.51.100.20", "after_ipv4": "203.0.113.21", "attempts": 1},
                },
                {
                    "id": "20000000-0000-4000-8000-000000000002",
                    "node_id": NODE_IDS["singapore"],
                    "action": "restart_xui",
                    "parameters": {},
                    "status": "succeeded",
                    "source": "scheduled",
                    "result": {"service": "x-ui.service"},
                },
                {
                    "id": "20000000-0000-4000-8000-000000000003",
                    "node_id": NODE_IDS["frankfurt"],
                    "action": "get_ip",
                    "parameters": {},
                    "status": "skipped_offline",
                    "source": "manual",
                    "error_code": "node_offline",
                    "error_message": "Agent is offline; request was not queued",
                    "result": {},
                },
                {
                    "id": "20000000-0000-4000-8000-000000000004",
                    "node_id": NODE_IDS["singapore"],
                    "action": "warp_on",
                    "parameters": {},
                    "status": "failed",
                    "source": "manual",
                    "error_code": "helper_failed",
                    "error_message": "Demo helper reported a recoverable error",
                    "result": {},
                },
            ]
            for item in requests:
                created = now - timedelta(minutes=len(requests) - requests.index(item) + 2)
                finished = iso(created + timedelta(seconds=8))
                connection.execute(
                    """
                    INSERT INTO action_requests
                      (id, node_id, action, parameters_json, source, status,
                       error_code, error_message, result_json, attempts,
                       issued_at, deadline_at, dispatched_at, accepted_at,
                       started_at, finished_at, created_at, updated_at)
                    VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
                    """,
                    (
                        item["id"],
                        item["node_id"],
                        item["action"],
                        json.dumps(item["parameters"], separators=(",", ":")),
                        item["source"],
                        item["status"],
                        item.get("error_code"),
                        item.get("error_message"),
                        json.dumps(item.get("result", {}), separators=(",", ":")),
                        1,
                        iso(created),
                        iso(created + timedelta(minutes=3)),
                        iso(created + timedelta(seconds=1)) if item["status"] != "skipped_offline" else None,
                        iso(created + timedelta(seconds=2)) if item["status"] != "skipped_offline" else None,
                        iso(created + timedelta(seconds=3)) if item["status"] != "skipped_offline" else None,
                        finished,
                        iso(created),
                        finished,
                    ),
                )
                if item["status"] in {"succeeded", "failed"}:
                    success = item["status"] == "succeeded"
                    connection.execute(
                        """
                        INSERT INTO action_results
                          (id, request_id, node_id, success, error_code, error_message, result_json, created_at)
                        VALUES (?, ?, ?, ?, ?, ?, ?, ?)
                        """,
                        (
                            str(uuid.uuid4()),
                            item["id"],
                            item["node_id"],
                            int(success),
                            item.get("error_code"),
                            item.get("error_message"),
                            json.dumps(item.get("result", {}), separators=(",", ":")),
                            finished,
                        ),
                    )

        fixture_path.parent.mkdir(parents=True, exist_ok=True)
        fixture_path.write_text(json.dumps({"agents": AGENTS}, indent=2), encoding="utf-8")
    finally:
        connection.close()


def main() -> None:
    parser = argparse.ArgumentParser(description="Seed disposable vps-tool panel demo data")
    parser.add_argument("--db", required=True, type=Path)
    parser.add_argument("--fixture", required=True, type=Path)
    args = parser.parse_args()
    insert_demo(args.db, args.fixture)
    print(f"Seeded demo database: {args.db}")
    print(f"Wrote mock Agent fixture: {args.fixture}")


if __name__ == "__main__":
    main()

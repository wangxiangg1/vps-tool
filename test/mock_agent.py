from __future__ import annotations

import argparse
import asyncio
import base64
import hashlib
import json
import os
import secrets
import struct
from datetime import datetime, timezone
from pathlib import Path
from urllib.parse import urlparse


CAPABILITIES = ["get_status", "get_ip", "warp_on", "warp_off", "change_ip", "restart_xui"]


def iso_now() -> str:
    return datetime.now(timezone.utc).isoformat(timespec="seconds").replace("+00:00", "Z")


def envelope(message_type: str, payload: dict[str, object]) -> dict[str, object]:
    return {
        "protocol_version": 1,
        "message_type": message_type,
        "message_id": secrets.token_hex(16),
        "sent_at": iso_now(),
        "payload": payload,
    }


class LocalWebSocket:
    def __init__(self, url: str):
        parsed = urlparse(url)
        if parsed.scheme != "ws" or not parsed.hostname or parsed.path != "/agent":
            raise ValueError("demo Agent only supports a ws://host:port/agent URL")
        self.host = parsed.hostname
        self.port = parsed.port or 80
        self.path = parsed.path
        self.reader: asyncio.StreamReader | None = None
        self.writer: asyncio.StreamWriter | None = None

    async def connect(self) -> None:
        self.reader, self.writer = await asyncio.open_connection(self.host, self.port)
        key = base64.b64encode(os.urandom(16)).decode("ascii")
        request = (
            f"GET {self.path} HTTP/1.1\r\n"
            f"Host: {self.host}:{self.port}\r\n"
            "Upgrade: websocket\r\n"
            "Connection: Upgrade\r\n"
            f"Sec-WebSocket-Key: {key}\r\n"
            "Sec-WebSocket-Version: 13\r\n\r\n"
        )
        self.writer.write(request.encode("ascii"))
        await self.writer.drain()
        response = await self.reader.readuntil(b"\r\n\r\n")
        if b" 101 " not in response.split(b"\r\n", 1)[0]:
            raise ConnectionError(f"WebSocket upgrade failed: {response[:200]!r}")
        expected = base64.b64encode(hashlib.sha1((key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11").encode()).digest()).decode("ascii")
        response_headers = {}
        for line in response.decode("latin1").split("\r\n")[1:]:
            if ":" in line:
                name, value = line.split(":", 1)
                response_headers[name.strip().lower()] = value.strip()
        if response_headers.get("sec-websocket-accept") != expected:
            raise ConnectionError("WebSocket accept key did not match")

    async def send_json(self, value: dict[str, object]) -> None:
        await self.send_frame(json.dumps(value, separators=(",", ":")).encode("utf-8"))

    async def send_frame(self, payload: bytes, opcode: int = 1) -> None:
        if not self.writer:
            raise ConnectionError("WebSocket is not connected")
        length = len(payload)
        header = bytearray([0x80 | opcode])
        if length < 126:
            header.append(0x80 | length)
        elif length <= 0xFFFF:
            header.append(0x80 | 126)
            header.extend(struct.pack("!H", length))
        else:
            header.append(0x80 | 127)
            header.extend(struct.pack("!Q", length))
        mask = os.urandom(4)
        header.extend(mask)
        masked = bytes(byte ^ mask[index % 4] for index, byte in enumerate(payload))
        self.writer.write(bytes(header) + masked)
        await self.writer.drain()

    async def receive_frame(self) -> tuple[int, bytes]:
        if not self.reader:
            raise ConnectionError("WebSocket is not connected")
        first, second = await self.reader.readexactly(2)
        opcode = first & 0x0F
        length = second & 0x7F
        if length == 126:
            length = struct.unpack("!H", await self.reader.readexactly(2))[0]
        elif length == 127:
            length = struct.unpack("!Q", await self.reader.readexactly(8))[0]
        if second & 0x80:
            mask = await self.reader.readexactly(4)
            payload = await self.reader.readexactly(length)
            payload = bytes(byte ^ mask[index % 4] for index, byte in enumerate(payload))
        else:
            payload = await self.reader.readexactly(length)
        return opcode, payload

    async def close(self) -> None:
        if self.writer:
            try:
                await self.send_frame(b"\x03\xe8", opcode=8)
            except Exception:
                pass
            self.writer.close()
            await self.writer.wait_closed()


async def send_status(ws: LocalWebSocket, agent: dict[str, object]) -> None:
    await ws.send_json(
        envelope(
            "status_report",
            {
                "node_id": agent["node_id"],
                "protocol_version": 1,
                "agent_version": agent["agent_version"],
                "hostname": agent["hostname"],
                "os_name": "Debian",
                "os_version": "12",
                "architecture": agent["architecture"],
                "cpu_percent": float(agent["cpu_percent"]),
                "memory_used_bytes": agent["memory_used_bytes"],
                "memory_total_bytes": agent["memory_total_bytes"],
                "root_used_bytes": agent["root_used_bytes"],
                "root_total_bytes": agent["root_total_bytes"],
                "uptime_seconds": agent["uptime_seconds"],
                "warp_status": agent["warp_status"],
                "xui_status": agent["xui_status"],
                "public_ipv4": agent["public_ipv4"],
                "public_ipv6": agent["public_ipv6"],
                "observed_at": iso_now(),
            },
        )
    )


async def run_agent(url: str, agent: dict[str, object]) -> None:
    while True:
        ws = LocalWebSocket(url)
        try:
            await ws.connect()
            await ws.send_json(
                envelope(
                    "agent_hello",
                    {
                        "node_id": agent["node_id"],
                        "credential": agent["credential"],
                        "protocol_version": 1,
                        "agent_version": agent["agent_version"],
                        "hostname": agent["hostname"],
                        "architecture": agent["architecture"],
                        "capabilities": CAPABILITIES,
                    },
                )
            )
            await send_status(ws, agent)

            async def heartbeat() -> None:
                while True:
                    await asyncio.sleep(15)
                    await ws.send_json(
                        envelope(
                            "heartbeat",
                            {
                                "node_id": agent["node_id"],
                                "warp_state": agent["warp_status"],
                                "xui_state": agent["xui_status"],
                                "uptime_seconds": agent["uptime_seconds"],
                                "status_collected_at": iso_now(),
                            },
                        )
                    )

            heartbeat_task = asyncio.create_task(heartbeat())
            try:
                while True:
                    opcode, raw = await ws.receive_frame()
                    if opcode == 8:
                        return
                    if opcode == 9:
                        await ws.send_frame(raw, opcode=10)
                        continue
                    if opcode != 1:
                        continue
                    message = json.loads(raw.decode("utf-8"))
                    if message.get("message_type") != "command":
                        continue
                    command = message.get("payload", {})
                    request_id = command.get("request_id")
                    action = command.get("action")
                    await ws.send_json(envelope("command_ack", {"request_id": request_id, "accepted": True}))
                    await ws.send_json(
                        envelope(
                            "command_progress",
                            {"request_id": request_id, "progress_percent": 100, "message": "demo Agent completed the action"},
                        )
                    )
                    if action == "warp_on":
                        agent["warp_status"] = "on"
                    elif action == "warp_off":
                        agent["warp_status"] = "off"
                    elif action == "restart_xui":
                        agent["xui_status"] = "running"
                    result: dict[str, object] = {"demo": True, "action": action}
                    if action == "get_ip":
                        result["egress_ipv4"] = agent["public_ipv4"]
                    if action == "change_ip":
                        result.update({"before_ipv4": agent["public_ipv4"], "after_ipv4": agent["public_ipv4"], "attempts": 1})
                    await ws.send_json(envelope("command_result", {"request_id": request_id, "success": True, "result": result}))
                    await send_status(ws, agent)
            finally:
                heartbeat_task.cancel()
                await asyncio.gather(heartbeat_task, return_exceptions=True)
        except (ConnectionError, asyncio.IncompleteReadError, OSError, json.JSONDecodeError) as exc:
            print(f"{agent['hostname']}: reconnecting after {exc}", flush=True)
        finally:
            await ws.close()
        await asyncio.sleep(2)


async def main_async(url: str, fixture_path: Path) -> None:
    fixture = json.loads(fixture_path.read_text(encoding="utf-8"))
    await asyncio.gather(*(run_agent(url, agent) for agent in fixture["agents"]))


def main() -> None:
    parser = argparse.ArgumentParser(description="Local disposable mock Agent for the vps-tool panel")
    parser.add_argument("--url", required=True)
    parser.add_argument("--fixture", required=True, type=Path)
    args = parser.parse_args()
    try:
        asyncio.run(main_async(args.url, args.fixture))
    except KeyboardInterrupt:
        pass


if __name__ == "__main__":
    main()

from __future__ import annotations

import asyncio
import json
import logging
import uuid
from datetime import datetime, timezone
from typing import Any

from fastapi import WebSocket, WebSocketDisconnect
from pydantic import ValidationError

from .action_service import ActionService
from .actions import SUPPORTED_ACTIONS
from .config import Settings
from .nodes import NodeService
from .schemas import (
    AgentEnvelope,
    AgentHelloPayload,
    CommandAckPayload,
    CommandProgressPayload,
    CommandResultPayload,
    HeartbeatPayload,
    StatusReportPayload,
)
from .timeutil import iso_now, utc_now


logger = logging.getLogger(__name__)


class AgentProtocolError(ValueError):
    def __init__(self, code: str, message: str):
        self.code = code
        self.message = message
        super().__init__(message)


class AgentConnection:
    def __init__(self, websocket: WebSocket, node_id: str):
        self.websocket = websocket
        self.node_id = node_id
        self.connected_at = utc_now()
        self.last_received_at = utc_now()
        self.send_lock = asyncio.Lock()

    async def send(self, message: dict[str, Any]) -> None:
        async with self.send_lock:
            await self.websocket.send_json(message)


class ConnectionManager:
    def __init__(self, settings: Settings, nodes: NodeService, actions: ActionService):
        self.settings = settings
        self.nodes = nodes
        self.actions = actions
        self._connections: dict[str, AgentConnection] = {}
        self._lock = asyncio.Lock()

    def online_node_ids(self) -> set[str]:
        threshold = utc_now().timestamp() - self.settings.heartbeat_timeout_seconds
        return {
            node_id
            for node_id, connection in self._connections.items()
            if connection.last_received_at.timestamp() >= threshold
        }

    def is_online(self, node_id: str) -> bool:
        return node_id in self.online_node_ids()

    async def register(self, connection: AgentConnection) -> None:
        old: AgentConnection | None = None
        async with self._lock:
            old = self._connections.get(connection.node_id)
            self._connections[connection.node_id] = connection
        if old and old is not connection:
            try:
                await old.websocket.close(code=4008, reason="replaced_by_new_connection")
            except Exception:
                pass

    async def remove(self, connection: AgentConnection) -> None:
        removed = False
        async with self._lock:
            if self._connections.get(connection.node_id) is connection:
                del self._connections[connection.node_id]
                removed = True
        if removed:
            self.actions.mark_unknown_for_node(connection.node_id)

    async def disconnect_node(self, node_id: str) -> None:
        async with self._lock:
            connection = self._connections.pop(node_id, None)
        if connection:
            self.actions.mark_unknown_for_node(node_id)
            try:
                await connection.websocket.close(code=4001, reason="credential_revoked")
            except Exception:
                pass

    async def send_command(self, row: Any) -> None:
        connection = self._connections.get(row["node_id"])
        if not connection or not self.is_online(row["node_id"]):
            raise ConnectionError("agent_offline")
        await connection.send(
            self.envelope(
                "command",
                {
                    "request_id": row["id"],
                    "node_id": row["node_id"],
                    "action": row["action"],
                    "issued_at": row["issued_at"],
                    "deadline_at": row["deadline_at"],
                    "parameters": json.loads(row["parameters_json"]),
                },
            )
        )

    @staticmethod
    def envelope(message_type: str, payload: dict[str, Any]) -> dict[str, Any]:
        return {
            "protocol_version": 1,
            "message_type": message_type,
            "message_id": str(uuid.uuid4()),
            "sent_at": iso_now(),
            "payload": payload,
        }

    async def send_error(self, websocket: WebSocket, code: str, message: str) -> None:
        try:
            await websocket.send_json(self.envelope("server_notice", {"error_code": code, "message": message}))
        except Exception:
            pass

    def _validate_envelope(self, raw: Any) -> AgentEnvelope:
        try:
            envelope = AgentEnvelope.model_validate(raw)
        except ValidationError as exc:
            raise AgentProtocolError("invalid_message", str(exc.errors(include_url=False))) from exc
        if envelope.protocol_version != self.settings.protocol_version:
            raise AgentProtocolError(
                "agent_version_incompatible",
                f"unsupported protocol_version {envelope.protocol_version}",
            )
        return envelope

    @staticmethod
    def _payload(model: type[Any], value: dict[str, Any]) -> dict[str, Any]:
        try:
            return model.model_validate(value).model_dump(exclude_none=True)
        except ValidationError as exc:
            raise AgentProtocolError("invalid_message", str(exc.errors(include_url=False))) from exc

    async def handle(self, websocket: WebSocket) -> None:
        await websocket.accept()
        connection: AgentConnection | None = None
        try:
            try:
                raw_hello = await asyncio.wait_for(websocket.receive_json(), timeout=10)
                hello_envelope = self._validate_envelope(raw_hello)
                if hello_envelope.message_type != "agent_hello":
                    raise AgentProtocolError("handshake_required", "first message must be agent_hello")
                hello = AgentHelloPayload.model_validate(hello_envelope.payload)
            except AgentProtocolError:
                raise
            except (ValidationError, WebSocketDisconnect, asyncio.TimeoutError) as exc:
                raise AgentProtocolError("handshake_failed", "invalid or missing agent handshake") from exc

            if hello.protocol_version is not None and hello.protocol_version != self.settings.protocol_version:
                raise AgentProtocolError(
                    "agent_version_incompatible",
                    f"unsupported agent protocol_version {hello.protocol_version}",
                )
            unsupported_capabilities = sorted(set(hello.capabilities) - set(SUPPORTED_ACTIONS))
            if unsupported_capabilities:
                raise AgentProtocolError(
                    "unsupported_capability",
                    f"unsupported agent capabilities: {', '.join(unsupported_capabilities)}",
                )

            header_credential = self._authorization_credential(websocket.headers.get("authorization"))
            if hello.registration_token and (hello.credential or header_credential):
                raise AgentProtocolError(
                    "handshake_failed",
                    "registration_token cannot be combined with a credential",
                )
            if not hello.registration_token and not (hello.credential or header_credential):
                raise AgentProtocolError(
                    "handshake_failed",
                    "credential or registration_token is required",
                )
            if hello.credential and header_credential and hello.credential != header_credential:
                raise AgentProtocolError(
                    "handshake_failed",
                    "credential sources do not match",
                )

            node: dict[str, Any] | None
            issued_credential: str | None = None
            if hello.registration_token:
                registration = self.nodes.consume_enrollment_token(hello.node_id, hello.registration_token)
                if not registration:
                    raise AgentProtocolError("registration_denied", "registration token is invalid or expired")
                node, issued_credential = registration
            else:
                node = self.nodes.authenticate_credential(
                    hello.node_id,
                    hello.credential or header_credential or "",
                )
                if not node:
                    raise AgentProtocolError("authentication_failed", "agent credential is invalid or revoked")

            connection = AgentConnection(websocket, node["id"])
            await self.register(connection)
            self.nodes.mark_seen(node["id"])
            await connection.send(
                self.envelope(
                    "server_notice",
                    {
                        "event": "agent_authenticated",
                        "node_id": node["id"],
                        "registered_credential": issued_credential,
                    },
                )
            )
            await self._reconcile(node["id"], hello.reconcile)
            await self.actions.dispatch_pending(node["id"])
            await self._message_loop(connection)
        except AgentProtocolError as exc:
            if connection is None:
                await self.send_error(websocket, exc.code, exc.message)
            else:
                await self.send_error(websocket, exc.code, exc.message)
            try:
                await websocket.close(code=1008, reason=exc.code)
            except Exception:
                pass
        except WebSocketDisconnect:
            pass
        except Exception:
            logger.exception("agent gateway connection failed")
            try:
                await websocket.close(code=1011, reason="gateway_error")
            except Exception:
                pass
        finally:
            if connection:
                await self.remove(connection)

    @staticmethod
    def _authorization_credential(value: str | None) -> str | None:
        if not value:
            return None
        scheme, separator, credential = value.partition(" ")
        if scheme.lower() != "bearer" or not separator or not credential:
            return None
        return credential.strip() or None

    async def _reconcile(self, node_id: str, values: list[dict[str, Any]]) -> None:
        for value in values:
            try:
                payload = self._payload(CommandResultPayload, value)
            except AgentProtocolError:
                continue
            self.actions.reconcile_result(node_id, payload)

    async def _message_loop(self, connection: AgentConnection) -> None:
        while True:
            raw = await connection.websocket.receive_json()
            connection.last_received_at = utc_now()
            envelope = self._validate_envelope(raw)
            if envelope.message_type == "heartbeat":
                payload = self._payload(HeartbeatPayload, envelope.payload)
                self._assert_node(connection, payload["node_id"])
                self.nodes.mark_seen(connection.node_id)
            elif envelope.message_type == "status_report":
                payload = self._payload(StatusReportPayload, envelope.payload)
                self._assert_node(connection, payload["node_id"])
                if payload.get("protocol_version") not in (None, self.settings.protocol_version):
                    raise AgentProtocolError(
                        "agent_version_incompatible",
                        f"unsupported status protocol_version {payload['protocol_version']}",
                    )
                self.nodes.apply_status(connection.node_id, payload, payload.get("observed_at"))
            elif envelope.message_type == "command_ack":
                payload = self._payload(CommandAckPayload, envelope.payload)
                self.actions.accept_request(connection.node_id, payload)
            elif envelope.message_type == "command_progress":
                payload = self._payload(CommandProgressPayload, envelope.payload)
                self.actions.progress_request(connection.node_id, payload)
            elif envelope.message_type == "command_result":
                payload = self._payload(CommandResultPayload, envelope.payload)
                self.actions.finish_request(
                    node_id=connection.node_id,
                    request_id=payload["request_id"],
                    success=payload["success"],
                    error_code=payload.get("error_code"),
                    error_message=payload.get("error_message"),
                    result=payload.get("result") or {},
                )
            else:
                await self.send_error(
                    connection.websocket,
                    "unsupported_message_type",
                    f"unsupported message_type: {envelope.message_type}",
                )

    @staticmethod
    def _assert_node(connection: AgentConnection, node_id: str) -> None:
        if node_id != connection.node_id:
            raise AgentProtocolError("node_mismatch", "message node_id does not match authenticated node")

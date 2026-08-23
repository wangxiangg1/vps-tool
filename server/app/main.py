from __future__ import annotations

import logging
import uuid
from contextlib import asynccontextmanager
from pathlib import Path
from typing import Any

from fastapi import Depends, FastAPI, HTTPException, Request, Response, WebSocket
from fastapi.staticfiles import StaticFiles

from .action_service import ActionService, RequestConflictError
from .actions import SUPPORTED_ACTIONS, ActionRequestBody, ActionValidationError
from .config import Settings
from .db import Database, SCHEMA_VERSION
from .gateway import ConnectionManager
from .nodes import NodeService, NodeValidationError
from .scheduler import ScheduleValidationError, Scheduler, TaskService
from .schemas import (
    LoginRequest,
    NodeCreateRequest,
    NodeUpdateRequest,
    RevokeCredentialRequest,
    TaskCreateRequest,
    TaskUpdateRequest,
)
from .security import (
    LoginRateLimiter,
    authenticate_admin,
    create_session,
    delete_session,
    ensure_admin,
    require_csrf,
    require_session,
    rotate_csrf_token,
)
from .timeutil import iso_now


logging.basicConfig(level=logging.INFO)
logger = logging.getLogger(__name__)


def _error(status_code: int, code: str, message: str) -> HTTPException:
    return HTTPException(status_code=status_code, detail={"code": code, "message": message})


def _admin_id(session: dict[str, Any]) -> str:
    return str(session["user_id"])


def build_app(settings: Settings | None = None) -> FastAPI:
    selected_settings = settings or Settings.from_env()
    database = Database(selected_settings.db_path)
    database.initialize()
    ensure_admin(database, selected_settings)
    nodes = NodeService(database, selected_settings)
    actions = ActionService(database, selected_settings, nodes)
    tasks = TaskService(database, nodes, actions)
    scheduler = Scheduler(database, tasks, selected_settings)
    gateway = ConnectionManager(selected_settings, nodes, actions)
    actions.attach_gateway(gateway)
    limiter = LoginRateLimiter()

    @asynccontextmanager
    async def lifespan(app: FastAPI):
        app.state.db = database
        app.state.settings = selected_settings
        app.state.nodes = nodes
        app.state.actions = actions
        app.state.tasks = tasks
        app.state.scheduler = scheduler
        app.state.gateway = gateway
        await scheduler.start()
        try:
            yield
        finally:
            await scheduler.stop()

    app = FastAPI(title="vps-tool control plane", version="0.1.0", lifespan=lifespan)

    @app.get("/api/health")
    async def health() -> dict[str, Any]:
        database_check = database.fetchone("SELECT 1 AS ok")["ok"] == 1
        return {
            "ok": database_check,
            "service": "ok" if database_check else "error",
            "schema_version": SCHEMA_VERSION,
            "online_agents": len(gateway.online_node_ids()),
            "timestamp": iso_now(),
        }

    @app.post("/api/auth/login")
    async def login(body: LoginRequest, request: Request, response: Response) -> dict[str, Any]:
        client_key = request.client.host if request.client else "unknown"
        if limiter.is_blocked(client_key):
            raise _error(429, "login_rate_limited", "too many failed login attempts")
        user = authenticate_admin(database, body.username, body.password)
        if not user:
            limiter.record_failure(client_key)
            database.insert_audit(
                audit_id=str(uuid.uuid4()),
                actor_type="anonymous",
                actor_id=None,
                event_type="login_failed",
                node_id=None,
                metadata={},
                created_at=iso_now(),
            )
            raise _error(401, "invalid_credentials", "username or password is incorrect")
        limiter.clear(client_key)
        session_token, csrf_token, expires_at = create_session(
            database, user["id"], selected_settings.session_ttl_seconds
        )
        database.insert_audit(
            audit_id=str(uuid.uuid4()),
            actor_type="admin",
            actor_id=user["id"],
            event_type="login_succeeded",
            node_id=None,
            metadata={},
            created_at=iso_now(),
        )
        response.set_cookie(
            selected_settings.cookie_name,
            session_token,
            httponly=True,
            secure=selected_settings.cookie_secure,
            samesite="strict",
            max_age=selected_settings.session_ttl_seconds,
            path="/",
        )
        return {
            "user": {"id": user["id"], "username": user["username"]},
            "csrf_token": csrf_token,
            "expires_at": expires_at,
        }

    @app.get("/api/auth/me")
    async def me(session: dict[str, Any] = Depends(require_session)) -> dict[str, Any]:
        return {
            "id": session["user_id"],
            "username": session["username"],
            "csrf_token": rotate_csrf_token(database, session),
            "expires_at": session["expires_at"],
        }

    @app.post("/api/auth/logout")
    async def logout(
        request: Request,
        response: Response,
        _: dict[str, Any] = Depends(require_csrf),
    ) -> dict[str, str]:
        delete_session(database, request.cookies.get(selected_settings.cookie_name))
        response.delete_cookie(selected_settings.cookie_name, path="/")
        return {"status": "ok"}

    @app.get("/api/actions")
    async def action_catalog_and_requests(
        node_id: str | None = None,
        _: dict[str, Any] = Depends(require_session),
    ) -> dict[str, Any]:
        return {
            "actions": [
                {
                    "name": name,
                    "state_changing": name in {"warp_on", "warp_off", "change_ip", "restart_xui"},
                }
                for name in SUPPORTED_ACTIONS
            ],
            "requests": actions.list_requests(node_id),
        }

    @app.get("/api/nodes")
    async def list_nodes(_: dict[str, Any] = Depends(require_session)) -> dict[str, Any]:
        return {"nodes": nodes.list_nodes(gateway.online_node_ids())}

    @app.post("/api/nodes")
    async def create_node(
        body: NodeCreateRequest,
        session: dict[str, Any] = Depends(require_csrf),
    ) -> dict[str, Any]:
        try:
            node, registration_token, expires_at = nodes.create(
                name=body.name,
                region=body.region,
                tags=list(body.tags),
                warp_adapter=body.warp_adapter,
                xui_service=body.xui_service,
                notes=body.notes,
                actor_id=_admin_id(session),
            )
        except NodeValidationError as exc:
            raise _error(422, "invalid_parameters", str(exc)) from exc
        return {
            "node": node,
            "registration_token": registration_token,
            "registration_token_expires_at": expires_at,
        }

    @app.get("/api/nodes/{node_id}")
    async def get_node(node_id: str, _: dict[str, Any] = Depends(require_session)) -> dict[str, Any]:
        node = nodes.get(node_id)
        if not node:
            raise _error(404, "node_not_found", "node was not found")
        node["online"] = gateway.is_online(node_id)
        return {"node": node}

    @app.patch("/api/nodes/{node_id}")
    async def update_node(
        node_id: str,
        body: NodeUpdateRequest,
        session: dict[str, Any] = Depends(require_csrf),
    ) -> dict[str, Any]:
        try:
            node = nodes.update(node_id, body.model_dump(exclude_none=True), _admin_id(session))
        except NodeValidationError as exc:
            raise _error(422, "invalid_parameters", str(exc)) from exc
        if not node:
            raise _error(404, "node_not_found", "node was not found")
        node["online"] = gateway.is_online(node_id)
        return {"node": node}

    @app.delete("/api/nodes/{node_id}")
    async def delete_node(node_id: str, session: dict[str, Any] = Depends(require_csrf)) -> dict[str, str]:
        if not nodes.soft_delete(node_id, _admin_id(session)):
            raise _error(404, "node_not_found", "node was not found")
        await gateway.disconnect_node(node_id)
        return {"status": "deleted"}

    @app.post("/api/nodes/{node_id}/enrollment-token")
    async def enrollment_token(
        node_id: str,
        session: dict[str, Any] = Depends(require_csrf),
    ) -> dict[str, str]:
        created = nodes.create_enrollment_token(node_id, _admin_id(session))
        if not created:
            raise _error(404, "node_not_found", "node was not found")
        token, expires_at = created
        return {"registration_token": token, "registration_token_expires_at": expires_at}

    @app.post("/api/nodes/{node_id}/credentials/rotate")
    async def rotate_credential(
        node_id: str,
        session: dict[str, Any] = Depends(require_csrf),
    ) -> dict[str, str]:
        credential = nodes.rotate_credential(node_id, _admin_id(session))
        if not credential:
            raise _error(404, "node_not_found", "node was not found")
        await gateway.disconnect_node(node_id)
        return {"agent_credential": credential}

    @app.post("/api/nodes/{node_id}/credentials/revoke")
    async def revoke_credential(
        node_id: str,
        body: RevokeCredentialRequest,
        session: dict[str, Any] = Depends(require_csrf),
    ) -> dict[str, str]:
        if not nodes.revoke_credentials(node_id, _admin_id(session), body.reason):
            raise _error(404, "node_not_found", "node was not found")
        await gateway.disconnect_node(node_id)
        return {"status": "revoked"}

    @app.post("/api/nodes/{node_id}/actions")
    async def create_action(
        node_id: str,
        body: ActionRequestBody,
        session: dict[str, Any] = Depends(require_csrf),
    ) -> dict[str, Any]:
        try:
            request_data = await actions.create_request(
                node_id=node_id,
                action=body.action,
                parameters=body.parameters,
                source="manual",
                actor_id=_admin_id(session),
                request_id=body.request_id,
                queue_if_offline=body.queue_if_offline,
            )
        except ActionValidationError as exc:
            raise _error(422, exc.code, exc.message) from exc
        except RequestConflictError as exc:
            code_status = 409 if exc.code in {"action_busy", "request_duplicate"} else 404
            raise _error(code_status, exc.code, exc.message) from exc
        return {"request": request_data}

    @app.get("/api/action-requests")
    async def list_action_requests(
        node_id: str | None = None,
        _: dict[str, Any] = Depends(require_session),
    ) -> dict[str, Any]:
        return {"requests": actions.list_requests(node_id)}

    @app.get("/api/action-requests/{request_id}")
    async def get_action_request(
        request_id: str,
        _: dict[str, Any] = Depends(require_session),
    ) -> dict[str, Any]:
        request_data = actions.get_request(request_id)
        if not request_data:
            raise _error(404, "request_not_found", "action request was not found")
        return {"request": request_data}

    @app.get("/api/tasks")
    async def list_tasks(_: dict[str, Any] = Depends(require_session)) -> dict[str, Any]:
        return {"tasks": tasks.list_tasks()}

    @app.post("/api/tasks")
    async def create_task(
        body: TaskCreateRequest,
        session: dict[str, Any] = Depends(require_csrf),
    ) -> dict[str, Any]:
        try:
            task = tasks.create_task(
                name=body.name,
                node_ids=list(body.node_ids),
                action=body.action,
                parameters=body.parameters,
                schedule_type=body.schedule_type,
                schedule_value=body.schedule_value,
                timezone_name=body.timezone,
                enabled=body.enabled,
                max_retries=body.max_retries,
                retry_intervals_seconds=list(body.retry_intervals_seconds),
                actor_id=_admin_id(session),
            )
        except (ActionValidationError, ScheduleValidationError) as exc:
            if isinstance(exc, ActionValidationError):
                code, message = exc.code, exc.message
            else:
                code, message = "invalid_schedule", str(exc)
            raise _error(422, code, message) from exc
        return {"task": task}

    @app.get("/api/tasks/{task_id}")
    async def get_task(task_id: str, _: dict[str, Any] = Depends(require_session)) -> dict[str, Any]:
        task = tasks.get_task(task_id)
        if not task:
            raise _error(404, "task_not_found", "task was not found")
        return {"task": task, "runs": tasks.list_runs(task_id)}

    @app.patch("/api/tasks/{task_id}")
    async def update_task(
        task_id: str,
        body: TaskUpdateRequest,
        session: dict[str, Any] = Depends(require_csrf),
    ) -> dict[str, Any]:
        try:
            task = tasks.update_task(task_id, body.model_dump(exclude_none=True), _admin_id(session))
        except ScheduleValidationError as exc:
            raise _error(422, "invalid_schedule", str(exc)) from exc
        if not task:
            raise _error(404, "task_not_found", "task was not found")
        return {"task": task}

    @app.delete("/api/tasks/{task_id}")
    async def delete_task(task_id: str, session: dict[str, Any] = Depends(require_csrf)) -> dict[str, str]:
        if not tasks.delete_task(task_id, _admin_id(session)):
            raise _error(404, "task_not_found", "task was not found")
        return {"status": "deleted"}

    @app.websocket("/agent")
    async def agent_socket(websocket: WebSocket) -> None:
        await gateway.handle(websocket)

    web_root = Path(__file__).resolve().parents[2] / "web"
    if web_root.is_dir():
        app.mount("/", StaticFiles(directory=web_root, html=True), name="web")

    return app


app = build_app()

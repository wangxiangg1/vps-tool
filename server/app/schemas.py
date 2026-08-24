from __future__ import annotations

from typing import Any, Literal

from pydantic import BaseModel, ConfigDict, Field, StrictBool, StrictFloat, StrictInt, StrictStr


class StrictModel(BaseModel):
    model_config = ConfigDict(extra="forbid", strict=True)


class LoginRequest(StrictModel):
    username: StrictStr = Field(min_length=1, max_length=128)
    password: StrictStr = Field(min_length=1, max_length=256)


class ChangePasswordRequest(StrictModel):
    current_password: StrictStr = Field(min_length=1, max_length=256)
    new_password: StrictStr = Field(min_length=1, max_length=256)
    confirm_password: StrictStr = Field(min_length=1, max_length=256)


class NodeCreateRequest(StrictModel):
    name: StrictStr = Field(min_length=1, max_length=100)
    region: StrictStr = Field(default="", max_length=100)
    tags: list[StrictStr] = Field(default_factory=list, max_length=20)
    warp_adapter: Literal["generic", "warp-cli", "wgcf"] = "generic"
    xui_service: StrictStr = Field(default="x-ui", min_length=1, max_length=128)
    notes: StrictStr = Field(default="", max_length=2000)


class NodeUpdateRequest(StrictModel):
    name: StrictStr | None = Field(default=None, min_length=1, max_length=100)
    region: StrictStr | None = Field(default=None, max_length=100)
    tags: list[StrictStr] | None = Field(default=None, max_length=20)
    warp_adapter: Literal["generic", "warp-cli", "wgcf"] | None = None
    xui_service: StrictStr | None = Field(default=None, min_length=1, max_length=128)
    notes: StrictStr | None = Field(default=None, max_length=2000)


class RevokeCredentialRequest(StrictModel):
    reason: StrictStr = Field(default="admin_revoked", max_length=200)


class AgentEnvelope(StrictModel):
    protocol_version: StrictInt
    message_type: StrictStr
    message_id: StrictStr
    sent_at: StrictStr
    payload: dict[str, Any]


class AgentHelloPayload(StrictModel):
    node_id: StrictStr
    credential: StrictStr | None = None
    registration_token: StrictStr | None = None
    protocol_version: StrictInt | None = None
    agent_version: StrictStr = Field(default="unknown", max_length=100)
    hostname: StrictStr | None = Field(default=None, max_length=255)
    architecture: StrictStr | None = Field(default=None, max_length=64)
    capabilities: list[StrictStr] = Field(default_factory=list, max_length=20)
    reconcile: list[dict[str, Any]] = Field(default_factory=list, max_length=200)


class HeartbeatPayload(StrictModel):
    node_id: StrictStr
    warp_state: Literal["on", "off", "degraded", "unknown"] | None = None
    xui_state: Literal["running", "stopped", "failed", "not_found", "unknown"] | None = None
    uptime_seconds: StrictInt | None = Field(default=None, ge=0)
    status_collected_at: StrictStr | None = None


class StatusReportPayload(StrictModel):
    node_id: StrictStr
    protocol_version: StrictInt | None = None
    agent_version: StrictStr | None = Field(default=None, max_length=100)
    hostname: StrictStr | None = Field(default=None, max_length=255)
    os_name: StrictStr | None = Field(default=None, max_length=100)
    os_version: StrictStr | None = Field(default=None, max_length=200)
    architecture: StrictStr | None = Field(default=None, max_length=64)
    cpu_percent: StrictFloat | None = Field(default=None, ge=0, le=100)
    memory_used_bytes: StrictInt | None = Field(default=None, ge=0)
    memory_total_bytes: StrictInt | None = Field(default=None, ge=0)
    root_used_bytes: StrictInt | None = Field(default=None, ge=0)
    root_total_bytes: StrictInt | None = Field(default=None, ge=0)
    uptime_seconds: StrictInt | None = Field(default=None, ge=0)
    warp_status: Literal["on", "off", "degraded", "unknown"] | None = None
    xui_status: Literal["running", "stopped", "failed", "not_found", "unknown"] | None = None
    public_ipv4: StrictStr | None = Field(default=None, max_length=64)
    public_ipv6: StrictStr | None = Field(default=None, max_length=128)
    observed_at: StrictStr | None = None


class CommandAckPayload(StrictModel):
    request_id: StrictStr
    accepted: StrictBool
    error_code: StrictStr | None = Field(default=None, max_length=100)
    error_message: StrictStr | None = Field(default=None, max_length=1000)


class CommandProgressPayload(StrictModel):
    request_id: StrictStr
    progress_percent: StrictInt = Field(ge=0, le=100)
    message: StrictStr | None = Field(default=None, max_length=1000)


class CommandResultPayload(StrictModel):
    request_id: StrictStr
    success: StrictBool
    error_code: StrictStr | None = Field(default=None, max_length=100)
    error_message: StrictStr | None = Field(default=None, max_length=2000)
    result: dict[str, Any] = Field(default_factory=dict)


class TaskCreateRequest(StrictModel):
    name: StrictStr = Field(min_length=1, max_length=100)
    node_ids: list[StrictStr] = Field(min_length=1, max_length=100)
    action: StrictStr
    parameters: dict[str, Any] = Field(default_factory=dict)
    schedule_type: Literal["daily", "weekly", "cron"]
    schedule_value: StrictStr = Field(min_length=1, max_length=100)
    timezone: StrictStr = Field(default="Asia/Shanghai", min_length=1, max_length=64)
    enabled: StrictBool = True
    max_retries: StrictInt = Field(default=2, ge=0, le=3)
    retry_intervals_seconds: list[StrictInt] = Field(default_factory=lambda: [30, 90], max_length=3)


class TaskUpdateRequest(StrictModel):
    name: StrictStr | None = Field(default=None, min_length=1, max_length=100)
    enabled: StrictBool | None = None
    schedule_type: Literal["daily", "weekly", "cron"] | None = None
    schedule_value: StrictStr | None = Field(default=None, min_length=1, max_length=100)
    timezone: StrictStr | None = Field(default=None, min_length=1, max_length=64)
    max_retries: StrictInt | None = Field(default=None, ge=0, le=3)
    retry_intervals_seconds: list[StrictInt] | None = Field(default=None, max_length=3)

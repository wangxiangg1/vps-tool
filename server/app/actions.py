from __future__ import annotations

from typing import Any, Literal, Type

from pydantic import BaseModel, ConfigDict, Field, StrictInt, StrictStr, ValidationError


SUPPORTED_ACTIONS = (
    "get_status",
    "get_ip",
    "warp_on",
    "warp_off",
    "change_ip",
    "restart_xui",
    "upgrade_agent",
)
STATE_CHANGING_ACTIONS = frozenset(
    {"warp_on", "warp_off", "change_ip", "restart_xui", "upgrade_agent"}
)
SCHEDULED_ACTIONS = frozenset({"warp_on", "warp_off", "change_ip", "restart_xui"})


class StrictModel(BaseModel):
    model_config = ConfigDict(extra="forbid", strict=True)


class EmptyParameters(StrictModel):
    pass


class ChangeIpParameters(StrictModel):
    max_attempts: StrictInt = Field(default=3, ge=1, le=3)
    timeout_seconds: StrictInt = Field(default=180, ge=30, le=180)


ACTION_PARAMETER_MODELS: dict[str, Type[BaseModel]] = {
    "get_status": EmptyParameters,
    "get_ip": EmptyParameters,
    "warp_on": EmptyParameters,
    "warp_off": EmptyParameters,
    "change_ip": ChangeIpParameters,
    "restart_xui": EmptyParameters,
    "upgrade_agent": EmptyParameters,
}


class ActionValidationError(ValueError):
    def __init__(self, code: str, message: str):
        self.code = code
        self.message = message
        super().__init__(message)


def validate_action(action: str, parameters: Any) -> dict[str, Any]:
    if action not in ACTION_PARAMETER_MODELS:
        raise ActionValidationError("unsupported_action", f"unsupported action: {action}")
    if not isinstance(parameters, dict):
        raise ActionValidationError("invalid_parameters", "parameters must be an object")
    model = ACTION_PARAMETER_MODELS[action]
    try:
        validated = model.model_validate(parameters)
    except ValidationError as exc:
        raise ActionValidationError("invalid_parameters", str(exc.errors(include_url=False))) from exc
    return validated.model_dump(mode="json", exclude_none=True)


class ActionRequestBody(StrictModel):
    action: StrictStr
    parameters: dict[str, Any] = Field(default_factory=dict)
    request_id: StrictStr | None = None
    queue_if_offline: bool = False


class TaskScheduleType(StrictModel):
    schedule_type: Literal["daily", "weekly", "cron"]
    schedule_value: StrictStr
    timezone: StrictStr = "Asia/Shanghai"

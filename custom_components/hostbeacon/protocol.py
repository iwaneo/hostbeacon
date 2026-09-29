"""Read and write the messages of the Agent-Integration protocol.

The shape of every message is in protocol/schema.json at the repo root.
Readers ignore unknown fields. A message with an unknown type becomes
Unknown, not an error, so the connection stays open.
"""

from __future__ import annotations

import dataclasses
import functools
import json
import types
from dataclasses import dataclass, field
from typing import Any, ClassVar, Literal, Union, get_args, get_origin, get_type_hints

PROTOCOL_VERSION = "1.0"
PROTOCOL_MAJORS = [1]

Kind = Literal["request", "reply", "event"]
Action = Literal["reboot", "update_run", "agent_update"]
RefusalReason = Literal[
    "disabled",
    "busy",
    "update_run_running",
    "too_soon_after_boot",
    "cannot_log",
    "duplicate",
    # Reserved, unused in v1.
    "not_allowed",
    "unknown_target",
]


class ProtocolError(Exception):
    """A frame is not valid JSON, has a bad envelope, or is a malformed known message."""


# --- State groups ------------------------------------------------------------


@dataclass(frozen=True, slots=True)
class AgentInfo:
    hostname: str
    agent_version: str
    newest_agent_version: str | None
    capabilities: list[str]
    enabled_actions: list[Action]


@dataclass(frozen=True, slots=True)
class System:
    cpu_percent: float | None
    memory_percent: float | None
    memory_used_bytes: int | None
    swap_percent: float | None
    load_1: float | None
    load_5: float | None
    load_15: float | None


@dataclass(frozen=True, slots=True)
class Mount:
    mount: str
    used_percent: float | None
    free_bytes: int | None
    total_bytes: int | None


@dataclass(frozen=True, slots=True)
class Disks:
    mounts: list[Mount]


@dataclass(frozen=True, slots=True)
class Interface:
    name: str
    rx_bytes_per_second: float | None
    tx_bytes_per_second: float | None
    rx_bytes_total: int | None
    tx_bytes_total: int | None


@dataclass(frozen=True, slots=True)
class Network:
    interfaces: list[Interface]


@dataclass(frozen=True, slots=True)
class Temperatures:
    cpu_celsius: float | None


@dataclass(frozen=True, slots=True)
class NameList:
    """A count of the full set plus a capped list of names."""

    count: int | None
    names: list[str]


@dataclass(frozen=True, slots=True)
class Container:
    name: str
    state: Literal["running", "stopped", "unhealthy"]


@dataclass(frozen=True, slots=True)
class Containers:
    count: int | None
    running: int | None
    stopped: int | None
    unhealthy: int | None
    items: list[Container]


@dataclass(frozen=True, slots=True)
class SmartDisk:
    device: str
    health: Literal["ok", "failing"] | None
    temperature_celsius: float | None
    wear_percent: float | None


@dataclass(frozen=True, slots=True)
class Smart:
    disks: list[SmartDisk]


@dataclass(frozen=True, slots=True)
class Package:
    name: str
    installed_version: str
    new_version: str


@dataclass(frozen=True, slots=True)
class AvailableUpdates:
    count: int | None
    packages: list[Package]
    fingerprint: str | None
    last_refresh: str | None


@dataclass(frozen=True, slots=True)
class UpdateRun:
    run_id: str | None
    state: Literal["idle", "waiting_for_lock", "running", "finished", "result_unknown"]
    percent: float | None
    started_at: str | None
    finished_at: str | None
    result: Literal["ok", "failed", "needs_manual_update"] | None
    installed: int | None
    remaining: int | None
    error: str | None
    needs_manual_update: NameList


@dataclass(frozen=True, slots=True)
class Flags:
    reboot_required: Literal["yes", "no", "unknown"]
    package_task_running: bool
    package_system_broken: bool
    package_system_fix_command: str | None
    last_boot: str | None


def _group() -> Any:
    """A group that is left out of the message when it is None."""
    return field(default=None, metadata={"omit_if_none": True})


@dataclass(frozen=True, slots=True)
class Groups:
    """State groups. None means the group is not in the message."""

    agent: AgentInfo | None = _group()
    system: System | None = _group()
    disks: Disks | None = _group()
    network: Network | None = _group()
    temperatures: Temperatures | None = _group()
    failed_services: NameList | None = _group()
    containers: Containers | None = _group()
    smart: Smart | None = _group()
    available_updates: AvailableUpdates | None = _group()
    update_run: UpdateRun | None = _group()
    flags: Flags | None = _group()


# --- Messages ----------------------------------------------------------------


@dataclass(frozen=True, slots=True)
class Distro:
    id: str | None
    name: str | None
    version: str | None


@dataclass(frozen=True, slots=True)
class HelloRequest:
    TYPE: ClassVar = "hello"
    KIND: ClassVar = "request"

    id: str
    protocol_version: str
    protocol_majors: list[int]
    instance_id: str
    run_id: str
    copied_from: list[str]
    hostname: str
    agent_version: str
    newest_agent_version: str | None
    capabilities: list[str]
    enabled_actions: list[Action]
    environment: Literal["bare_metal", "vm", "lxc"] | None
    distro: Distro
    architecture: str
    kernel: str | None


@dataclass(frozen=True, slots=True)
class HelloReply:
    TYPE: ClassVar = "hello"
    KIND: ClassVar = "reply"

    id: str
    reply_to: str
    integration_version: str
    protocol_version: str
    protocol_majors: list[int]


@dataclass(frozen=True, slots=True)
class Snapshot:
    TYPE: ClassVar = "snapshot"
    KIND: ClassVar = "event"

    id: str
    groups: Groups


@dataclass(frozen=True, slots=True)
class Delta:
    TYPE: ClassVar = "delta"
    KIND: ClassVar = "event"

    id: str
    groups: Groups


@dataclass(frozen=True, slots=True)
class ActionRequest:
    TYPE: ClassVar = "action_request"
    KIND: ClassVar = "request"

    id: str
    action_id: str
    action: Action
    user: str | None


@dataclass(frozen=True, slots=True)
class ActionOutcome:
    result: Literal["ok", "failed"]
    error: str | None


@dataclass(frozen=True, slots=True)
class ActionAck:
    TYPE: ClassVar = "action_ack"
    KIND: ClassVar = "reply"

    id: str
    reply_to: str
    action_id: str
    status: Literal["accepted", "refused"]
    reason: RefusalReason | None
    first_result: ActionOutcome | None


@dataclass(frozen=True, slots=True)
class ActionResult:
    TYPE: ClassVar = "action_result"
    KIND: ClassVar = "event"

    id: str
    action_id: str
    action: Action
    result: Literal["ok", "failed"]
    error: str | None


@dataclass(frozen=True, slots=True)
class PairingRemoveRequest:
    TYPE: ClassVar = "pairing_remove"
    KIND: ClassVar = "request"

    id: str


@dataclass(frozen=True, slots=True)
class PairingRemoveReply:
    TYPE: ClassVar = "pairing_remove"
    KIND: ClassVar = "reply"

    id: str
    reply_to: str


@dataclass(frozen=True, slots=True)
class Unsupported:
    TYPE: ClassVar = "unsupported"
    KIND: ClassVar = "reply"

    id: str
    reply_to: str
    request_type: str


@dataclass(frozen=True, slots=True)
class Unknown:
    """A message whose type this version does not know. Only the envelope is kept."""

    type: str
    id: str
    kind: Kind


Message = (
    HelloRequest
    | HelloReply
    | Snapshot
    | Delta
    | ActionRequest
    | ActionAck
    | ActionResult
    | PairingRemoveRequest
    | PairingRemoveReply
    | Unsupported
    | Unknown
)

_MESSAGE_CLASSES = {
    (cls.TYPE, cls.KIND): cls
    for cls in (
        HelloRequest,
        HelloReply,
        Snapshot,
        Delta,
        ActionRequest,
        ActionAck,
        ActionResult,
        PairingRemoveRequest,
        PairingRemoveReply,
        Unsupported,
    )
}
_KNOWN_TYPES = {message_type for message_type, _ in _MESSAGE_CLASSES}


def decode(frame: str | bytes) -> Message:
    """Read one frame. Raises ProtocolError if it is malformed."""
    try:
        data = json.loads(frame)
    except ValueError as err:
        raise ProtocolError(f"not JSON: {err}") from None
    if not isinstance(data, dict):
        raise ProtocolError("a message must be a JSON object")

    message_type = _read(str, data.get("type"), "type")
    message_id = _read(str, data.get("id"), "id")
    kind = _read(Kind, data.get("kind"), "kind")
    if not message_type or not message_id:
        raise ProtocolError("type and id must not be empty")
    if kind == "reply":
        _read(str, data.get("reply_to"), "reply_to")

    if message_type not in _KNOWN_TYPES:
        return Unknown(type=message_type, id=message_id, kind=kind)
    cls = _MESSAGE_CLASSES.get((message_type, kind))
    if cls is None:
        raise ProtocolError(f"{message_type} cannot be a {kind}")
    return _read(cls, data, message_type)


def encode(message: Message) -> str:
    """Write one frame."""
    if isinstance(message, Unknown):
        raise ProtocolError("an Unknown message cannot be sent")
    data = {"type": message.TYPE, "kind": message.KIND, **_write(message)}
    return json.dumps(data, ensure_ascii=False, separators=(",", ":"))


def unsupported_reply(message: Unknown, reply_id: str) -> Unsupported | None:
    """The reply to an unknown message: unsupported for a request, else nothing."""
    if message.kind != "request":
        return None
    return Unsupported(id=reply_id, reply_to=message.id, request_type=message.type)


# --- Conversion between JSON values and the classes above --------------------


@functools.cache
def _hints(cls: type) -> dict[str, Any]:
    return get_type_hints(cls)


def _read(tp: Any, value: Any, path: str) -> Any:
    origin = get_origin(tp)
    if origin in (Union, types.UnionType):
        options = get_args(tp)
        if value is None and type(None) in options:
            return None
        (tp,) = [option for option in options if option is not type(None)]
        origin = get_origin(tp)

    if dataclasses.is_dataclass(tp):
        if not isinstance(value, dict):
            raise ProtocolError(f"{path} must be an object")
        values = {}
        for item in dataclasses.fields(tp):
            if item.name in value:
                values[item.name] = _read(_hints(tp)[item.name], value[item.name], f"{path}.{item.name}")
            elif item.default is dataclasses.MISSING:
                raise ProtocolError(f"{path}.{item.name} is missing")
        return tp(**values)
    if origin is list:
        if not isinstance(value, list):
            raise ProtocolError(f"{path} must be a list")
        (item_type,) = get_args(tp)
        return [_read(item_type, item, f"{path}[{index}]") for index, item in enumerate(value)]
    if origin is Literal:
        if value not in get_args(tp):
            raise ProtocolError(f"{path} has an unknown value {value!r}")
        return value
    if tp is float:
        valid = isinstance(value, (int, float)) and not isinstance(value, bool)
    elif tp is int:
        valid = isinstance(value, int) and not isinstance(value, bool)
    else:
        valid = isinstance(value, tp)
    if not valid:
        raise ProtocolError(f"{path} must be {tp.__name__}")
    return value


def _write(value: Any) -> Any:
    if dataclasses.is_dataclass(value):
        return {
            item.name: _write(getattr(value, item.name))
            for item in dataclasses.fields(value)
            if not (item.metadata.get("omit_if_none") and getattr(value, item.name) is None)
        }
    if isinstance(value, list):
        return [_write(item) for item in value]
    return value

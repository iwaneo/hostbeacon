"""Read and write the messages of the Agent-Integration protocol.

The shape of every message is in protocol/schema.json at the repo root.
Readers ignore unknown fields. A message with an unknown type becomes
Unknown, not an error, so the connection stays open.
"""

from __future__ import annotations

import dataclasses
import functools
import json
import math
import re
import types
from dataclasses import dataclass, field
from typing import (
    Annotated,
    Any,
    ClassVar,
    Literal,
    Union,
    get_args,
    get_origin,
    get_type_hints,
)

PROTOCOL_VERSION = "1.1"
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


@dataclass(frozen=True, slots=True)
class _Pattern:
    """Text that must match a pattern."""

    name: str
    regex: re.Pattern[str]


@dataclass(frozen=True, slots=True)
class _Size:
    """A list with at most max_items items, or an object with at least min_keys keys."""

    max_items: int | None = None
    min_keys: int | None = None


# The same rules as the uuid and time patterns in schema.json.
Uuid = Annotated[
    str,
    _Pattern("a lowercase UUID", re.compile(r"[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}")),
]
Time = Annotated[
    str,
    _Pattern("a UTC time ending in Z", re.compile(r"[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(\.[0-9]+)?Z")),
]
# Name lists are capped at 100 items.
CAP = _Size(max_items=100)
# Whole numbers must fit a signed 64-bit integer, as in the Agent.
INT64 = range(-(2**63), 2**63)


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
    names: Annotated[list[str], CAP]


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
    items: Annotated[list[Container], CAP]


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
    packages: Annotated[list[Package], CAP]
    fingerprint: str | None
    last_refresh: Time | None


@dataclass(frozen=True, slots=True)
class UpdateRun:
    run_id: Uuid | None
    state: Literal["idle", "waiting_for_lock", "running", "finished", "result_unknown"]
    percent: float | None
    started_at: Time | None
    finished_at: Time | None
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
    last_boot: Time | None


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
    instance_id: Uuid
    run_id: Uuid
    copied_from: list[Uuid]
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
    # The Host ID Home Assistant knows this Agent as. Since 1.1.
    host_id: Uuid | None = field(default=None, metadata={"omit_if_none": True})


@dataclass(frozen=True, slots=True)
class Snapshot:
    TYPE: ClassVar = "snapshot"
    KIND: ClassVar = "event"

    id: str
    groups: Groups

    def __post_init__(self) -> None:
        for name in ("agent", "system", "update_run", "flags"):
            if getattr(self.groups, name) is None:
                raise ProtocolError(f"a snapshot needs the {name} group")


@dataclass(frozen=True, slots=True)
class Delta:
    TYPE: ClassVar = "delta"
    KIND: ClassVar = "event"

    id: str
    groups: Annotated[Groups, _Size(min_keys=1)]


@dataclass(frozen=True, slots=True)
class ActionRequest:
    TYPE: ClassVar = "action_request"
    KIND: ClassVar = "request"

    id: str
    action_id: Uuid
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
    action_id: Uuid
    status: Literal["accepted", "refused"]
    reason: RefusalReason | None
    first_result: ActionOutcome | None

    def __post_init__(self) -> None:
        if (self.status == "refused") != (self.reason is not None):
            raise ProtocolError("a refusal needs a reason, and an acceptance has none")
        if self.first_result is not None and self.reason != "duplicate":
            raise ProtocolError("only a duplicate carries the first result")


@dataclass(frozen=True, slots=True)
class ActionResult:
    TYPE: ClassVar = "action_result"
    KIND: ClassVar = "event"

    id: str
    action_id: Uuid
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

_MESSAGE_CLASSES = {(cls.TYPE, cls.KIND): cls for cls in get_args(Message) if cls is not Unknown}
_KNOWN_TYPES = {message_type for message_type, _ in _MESSAGE_CLASSES}


def decode(frame: str | bytes) -> Message:
    """Read one frame. Raises ProtocolError if it is malformed."""
    try:
        data = json.loads(frame, parse_constant=_refuse_constant)
    except ValueError as err:
        raise ProtocolError(f"not JSON: {err}") from None
    if not isinstance(data, dict):
        raise ProtocolError("a message must be a JSON object")
    _check_key_case(data, ("type", "id", "kind", "reply_to"), "message")

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
    """Write one frame. Raises ProtocolError if decode would refuse it."""
    if isinstance(message, Unknown):
        raise ProtocolError("an Unknown message cannot be sent")
    data = {"type": message.TYPE, "kind": message.KIND, **_write(message)}
    frame = json.dumps(data, ensure_ascii=False, separators=(",", ":"))
    decode(frame)
    return frame


def unsupported_reply(message: Unknown, reply_id: str) -> Unsupported | None:
    """The reply to an unknown message: unsupported for a request, else nothing."""
    if message.kind != "request":
        return None
    return Unsupported(id=reply_id, reply_to=message.id, request_type=message.type)


# --- Conversion between JSON values and the classes above --------------------


@functools.cache
def _hints(cls: type) -> dict[str, Any]:
    return get_type_hints(cls, include_extras=True)


def _refuse_constant(name: str) -> None:
    raise ProtocolError(f"{name} is not JSON")


def _check_key_case(value: dict, names: Any, path: str) -> None:
    """Refuse a key that differs from a known name only by case."""
    lower_names = {name.lower() for name in names}
    for key in value:
        if key not in names and key.lower() in lower_names:
            raise ProtocolError(f"{path} has a key with the wrong case: {key}")


def _read(tp: Any, value: Any, path: str) -> Any:
    origin = get_origin(tp)
    if origin in (Union, types.UnionType):
        options = get_args(tp)
        if value is None and type(None) in options:
            return None
        (tp,) = [option for option in options if option is not type(None)]
        origin = get_origin(tp)

    rules = ()
    if origin is Annotated:
        tp, *rules = get_args(tp)
        origin = get_origin(tp)
    for rule in rules:
        _check_rule(rule, value, path)

    if dataclasses.is_dataclass(tp):
        if not isinstance(value, dict):
            raise ProtocolError(f"{path} must be an object")
        fields = dataclasses.fields(tp)
        _check_key_case(value, {item.name for item in fields}, path)
        values = {}
        for item in fields:
            if item.name in value:
                if value[item.name] is None and item.metadata.get("omit_if_none"):
                    raise ProtocolError(f"{path}.{item.name} is null; leave it out instead")
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
        valid = isinstance(value, (int, float)) and not isinstance(value, bool) and math.isfinite(value)
    elif tp is int:
        valid = isinstance(value, int) and not isinstance(value, bool) and value in INT64
    else:
        valid = isinstance(value, tp)
    if not valid:
        raise ProtocolError(f"{path} must be {tp.__name__}")
    return value


def _check_rule(rule: Any, value: Any, path: str) -> None:
    if isinstance(rule, _Pattern):
        if isinstance(value, str) and not rule.regex.fullmatch(value):
            raise ProtocolError(f"{path} must be {rule.name}")
    elif isinstance(rule, _Size):
        if rule.max_items is not None and isinstance(value, list) and len(value) > rule.max_items:
            raise ProtocolError(f"{path} has more than {rule.max_items} items")
        if rule.min_keys is not None and isinstance(value, dict) and len(value) < rule.min_keys:
            raise ProtocolError(f"{path} needs at least {rule.min_keys} keys")


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

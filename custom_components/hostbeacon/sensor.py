"""Sensors for a Host. Each one appears only when the Host reports its capability."""

from __future__ import annotations

from collections.abc import Callable, Iterator
from dataclasses import dataclass
from datetime import datetime
from typing import Any

from homeassistant.components.sensor import (
    SensorDeviceClass,
    SensorEntity,
    SensorEntityDescription,
    SensorStateClass,
)
from homeassistant.const import (
    PERCENTAGE,
    EntityCategory,
    UnitOfDataRate,
    UnitOfInformation,
    UnitOfTemperature,
)
from homeassistant.core import HomeAssistant, callback
from homeassistant.helpers.entity_platform import AddConfigEntryEntitiesCallback
from homeassistant.helpers.typing import StateType
from homeassistant.util import dt as dt_util

from . import HostbeaconConfigEntry
from .connection import HostConnection
from .entity import HostEntity
from .protocol import Containers, Interface, Mount, SmartDisk

HOST_STATUS_OPTIONS = ["online", "updating", "rebooting", "offline"]
REBOOT_REQUIRED_OPTIONS = ["yes", "no"]
ENVIRONMENT_OPTIONS = ["bare_metal", "vm", "lxc"]


@dataclass(frozen=True, kw_only=True)
class HostSensorDescription(SensorEntityDescription):
    """A sensor read from the latest state of the Host."""

    value: Callable[[HostConnection], StateType | datetime]
    # False while the thing the sensor shows is gone, for example a mount.
    exists: Callable[[HostConnection], bool] = lambda connection: True
    # Name lists go here. They are never recorded (v1 spec §7.4).
    attributes: Callable[[HostConnection], dict[str, Any]] | None = None


def _timestamp(text: str | None) -> datetime | None:
    return dt_util.parse_datetime(text) if text else None


def _has(capability: str) -> Callable[[HostConnection], bool]:
    return lambda connection: capability in connection.capabilities


SYSTEM_SENSORS = (
    HostSensorDescription(
        key="cpu_usage",
        translation_key="cpu_usage",
        native_unit_of_measurement=PERCENTAGE,
        state_class=SensorStateClass.MEASUREMENT,
        suggested_display_precision=0,
        value=lambda c: c.groups.system.cpu_percent if c.groups.system else None,
    ),
    HostSensorDescription(
        key="memory_usage",
        translation_key="memory_usage",
        native_unit_of_measurement=PERCENTAGE,
        state_class=SensorStateClass.MEASUREMENT,
        suggested_display_precision=0,
        value=lambda c: c.groups.system.memory_percent if c.groups.system else None,
    ),
    HostSensorDescription(
        key="swap_usage",
        translation_key="swap_usage",
        native_unit_of_measurement=PERCENTAGE,
        state_class=SensorStateClass.MEASUREMENT,
        suggested_display_precision=0,
        entity_registry_enabled_default=False,
        value=lambda c: c.groups.system.swap_percent if c.groups.system else None,
    ),
    HostSensorDescription(
        key="last_boot",
        translation_key="last_boot",
        device_class=SensorDeviceClass.TIMESTAMP,
        value=lambda c: _timestamp(c.groups.flags.last_boot) if c.groups.flags else None,
    ),
    HostSensorDescription(
        key="reboot_required",
        translation_key="reboot_required",
        device_class=SensorDeviceClass.ENUM,
        options=REBOOT_REQUIRED_OPTIONS,
        # "unknown" is shown as HA's unknown state.
        value=lambda c: (
            c.groups.flags.reboot_required
            if c.groups.flags and c.groups.flags.reboot_required in REBOOT_REQUIRED_OPTIONS
            else None
        ),
    ),
    HostSensorDescription(
        key="environment",
        translation_key="environment",
        device_class=SensorDeviceClass.ENUM,
        options=ENVIRONMENT_OPTIONS,
        entity_category=EntityCategory.DIAGNOSTIC,
        entity_registry_enabled_default=False,
        value=lambda c: c.hello.environment if c.hello else None,
    ),
    HostSensorDescription(
        key="kernel",
        translation_key="kernel",
        entity_category=EntityCategory.DIAGNOSTIC,
        entity_registry_enabled_default=False,
        value=lambda c: c.hello.kernel if c.hello else None,
    ),
    HostSensorDescription(
        key="protocol_version",
        translation_key="protocol_version",
        entity_category=EntityCategory.DIAGNOSTIC,
        entity_registry_enabled_default=False,
        value=lambda c: c.hello.protocol_version if c.hello else None,
    ),
)

CAPABILITY_SENSORS = (
    HostSensorDescription(
        key="load_1",
        translation_key="load_1",
        state_class=SensorStateClass.MEASUREMENT,
        suggested_display_precision=2,
        exists=_has("load"),
        value=lambda c: c.groups.system.load_1 if c.groups.system else None,
    ),
    HostSensorDescription(
        key="load_5",
        translation_key="load_5",
        state_class=SensorStateClass.MEASUREMENT,
        suggested_display_precision=2,
        entity_registry_enabled_default=False,
        exists=_has("load"),
        value=lambda c: c.groups.system.load_5 if c.groups.system else None,
    ),
    HostSensorDescription(
        key="load_15",
        translation_key="load_15",
        state_class=SensorStateClass.MEASUREMENT,
        suggested_display_precision=2,
        entity_registry_enabled_default=False,
        exists=_has("load"),
        value=lambda c: c.groups.system.load_15 if c.groups.system else None,
    ),
    HostSensorDescription(
        key="cpu_temperature",
        translation_key="cpu_temperature",
        device_class=SensorDeviceClass.TEMPERATURE,
        native_unit_of_measurement=UnitOfTemperature.CELSIUS,
        state_class=SensorStateClass.MEASUREMENT,
        suggested_display_precision=0,
        exists=_has("temperatures"),
        value=lambda c: c.groups.temperatures.cpu_celsius if c.groups.temperatures else None,
    ),
    HostSensorDescription(
        key="failed_services",
        translation_key="failed_services",
        state_class=SensorStateClass.MEASUREMENT,
        exists=_has("failed_services"),
        value=lambda c: c.groups.failed_services.count if c.groups.failed_services else None,
        attributes=lambda c: {"services": c.groups.failed_services.names if c.groups.failed_services else []},
    ),
    *(
        HostSensorDescription(
            key=f"containers_{container_state}",
            translation_key=f"containers_{container_state}",
            state_class=SensorStateClass.MEASUREMENT,
            exists=_has("containers"),
            value=lambda c, container_state=container_state: (
                getattr(c.groups.containers, container_state) if c.groups.containers else None
            ),
            attributes=lambda c, container_state=container_state: {
                "containers": _container_names(c.groups.containers, container_state)
            },
        )
        for container_state in ("running", "stopped", "unhealthy")
    ),
    HostSensorDescription(
        key="available_updates",
        translation_key="available_updates",
        state_class=SensorStateClass.MEASUREMENT,
        exists=_has("available_updates"),
        value=lambda c: c.groups.available_updates.count if c.groups.available_updates else None,
    ),
)


def _container_names(containers: Containers | None, container_state: str) -> list[str]:
    return [item.name for item in containers.items if item.state == container_state] if containers else []


def smart_disk(connection: HostConnection, device: str) -> SmartDisk | None:
    smart = connection.groups.smart if "smart" in connection.capabilities else None
    return next((disk for disk in smart.disks if disk.device == device), None) if smart else None


def _mount(connection: HostConnection, path: str) -> Mount | None:
    disks = connection.groups.disks if "disks" in connection.capabilities else None
    return next((mount for mount in disks.mounts if mount.mount == path), None) if disks else None


def _interface(connection: HostConnection, name: str) -> Interface | None:
    network = connection.groups.network if "network" in connection.capabilities else None
    return next((item for item in network.interfaces if item.name == name), None) if network else None


def _mount_sensors(path: str) -> Iterator[HostSensorDescription]:
    def exists(connection: HostConnection) -> bool:
        return _mount(connection, path) is not None

    yield HostSensorDescription(
        key=f"disk_used_{path}",
        translation_key="disk_used",
        translation_placeholders={"mount": path},
        native_unit_of_measurement=PERCENTAGE,
        state_class=SensorStateClass.MEASUREMENT,
        suggested_display_precision=0,
        exists=exists,
        value=lambda c: mount.used_percent if (mount := _mount(c, path)) else None,
    )
    yield HostSensorDescription(
        key=f"disk_free_{path}",
        translation_key="disk_free",
        translation_placeholders={"mount": path},
        device_class=SensorDeviceClass.DATA_SIZE,
        native_unit_of_measurement=UnitOfInformation.BYTES,
        suggested_unit_of_measurement=UnitOfInformation.GIGABYTES,
        state_class=SensorStateClass.MEASUREMENT,
        suggested_display_precision=1,
        entity_registry_enabled_default=False,
        exists=exists,
        value=lambda c: mount.free_bytes if (mount := _mount(c, path)) else None,
    )


def _interface_sensors(name: str) -> Iterator[HostSensorDescription]:
    def exists(connection: HostConnection) -> bool:
        return _interface(connection, name) is not None

    for key, field in (("download", "rx_bytes_per_second"), ("upload", "tx_bytes_per_second")):
        yield HostSensorDescription(
            key=f"{key}_{name}",
            translation_key=key,
            translation_placeholders={"interface": name},
            device_class=SensorDeviceClass.DATA_RATE,
            native_unit_of_measurement=UnitOfDataRate.BYTES_PER_SECOND,
            suggested_unit_of_measurement=UnitOfDataRate.MEGABITS_PER_SECOND,
            state_class=SensorStateClass.MEASUREMENT,
            suggested_display_precision=2,
            exists=exists,
            value=lambda c, field=field: getattr(item, field) if (item := _interface(c, name)) else None,
        )
    for key, field in (("downloaded", "rx_bytes_total"), ("uploaded", "tx_bytes_total")):
        yield HostSensorDescription(
            key=f"{key}_{name}",
            translation_key=key,
            translation_placeholders={"interface": name},
            device_class=SensorDeviceClass.DATA_SIZE,
            native_unit_of_measurement=UnitOfInformation.BYTES,
            suggested_unit_of_measurement=UnitOfInformation.GIGABYTES,
            state_class=SensorStateClass.TOTAL_INCREASING,
            suggested_display_precision=1,
            entity_registry_enabled_default=False,
            exists=exists,
            value=lambda c, field=field: getattr(item, field) if (item := _interface(c, name)) else None,
        )


def _smart_sensors(disk: SmartDisk) -> Iterator[HostSensorDescription]:
    device = disk.device

    def exists(connection: HostConnection) -> bool:
        return smart_disk(connection, device) is not None

    yield HostSensorDescription(
        key=f"disk_temperature_{device}",
        translation_key="disk_temperature",
        translation_placeholders={"device": device},
        device_class=SensorDeviceClass.TEMPERATURE,
        native_unit_of_measurement=UnitOfTemperature.CELSIUS,
        state_class=SensorStateClass.MEASUREMENT,
        suggested_display_precision=0,
        exists=exists,
        value=lambda c: item.temperature_celsius if (item := smart_disk(c, device)) else None,
    )
    # Wear only for SSDs: other disks never report it.
    if disk.wear_percent is not None:
        yield HostSensorDescription(
            key=f"disk_wear_{device}",
            translation_key="disk_wear",
            translation_placeholders={"device": device},
            native_unit_of_measurement=PERCENTAGE,
            state_class=SensorStateClass.MEASUREMENT,
            suggested_display_precision=0,
            exists=exists,
            value=lambda c: item.wear_percent if (item := smart_disk(c, device)) else None,
        )


def _descriptions(connection: HostConnection) -> Iterator[HostSensorDescription]:
    """Every sensor the Host has now: fixed ones, then one per capability, mount, and interface."""
    yield from SYSTEM_SENSORS
    yield from (description for description in CAPABILITY_SENSORS if description.exists(connection))
    if "disks" in connection.capabilities and connection.groups.disks:
        for mount in connection.groups.disks.mounts:
            yield from _mount_sensors(mount.mount)
    if "network" in connection.capabilities and connection.groups.network:
        for item in connection.groups.network.interfaces:
            yield from _interface_sensors(item.name)
    if "smart" in connection.capabilities and connection.groups.smart:
        for disk in connection.groups.smart.disks:
            yield from _smart_sensors(disk)


async def async_setup_entry(
    hass: HomeAssistant, entry: HostbeaconConfigEntry, async_add_entities: AddConfigEntryEntitiesCallback
) -> None:
    """Add the sensors of one Host, and new ones when the Host reports them."""
    connection = entry.runtime_data
    added: set[str] = set()

    @callback
    def add_new_sensors() -> None:
        new = [description for description in _descriptions(connection) if description.key not in added]
        added.update(description.key for description in new)
        async_add_entities(HostSensor(entry, connection, description) for description in new)

    async_add_entities([HostStatusSensor(entry, connection), LastSeenSensor(entry, connection)])
    add_new_sensors()
    entry.async_on_unload(connection.add_listener(add_new_sensors))


class HostStatusSensor(HostEntity, SensorEntity):
    """Host status. Always available, so it can show Offline."""

    _attr_device_class = SensorDeviceClass.ENUM
    _attr_options = HOST_STATUS_OPTIONS
    _attr_translation_key = "host_status"

    def __init__(self, entry: HostbeaconConfigEntry, connection: HostConnection) -> None:
        super().__init__(entry, connection, "host_status")

    @property
    def available(self) -> bool:
        return True

    @property
    def native_value(self) -> str:
        return "online" if self._connection.online else "offline"


class LastSeenSensor(HostEntity, SensorEntity):
    """When the Agent was last seen. Always available, so it shows when an Offline Host was last there."""

    _attr_device_class = SensorDeviceClass.TIMESTAMP
    _attr_entity_category = EntityCategory.DIAGNOSTIC
    _attr_entity_registry_enabled_default = False
    _attr_translation_key = "last_seen"

    def __init__(self, entry: HostbeaconConfigEntry, connection: HostConnection) -> None:
        super().__init__(entry, connection, "last_seen")

    @property
    def available(self) -> bool:
        return True

    @property
    def native_value(self) -> datetime | None:
        return self._connection.last_seen


class HostSensor(HostEntity, SensorEntity):
    """A value from the Host's latest state."""

    entity_description: HostSensorDescription
    # Names of services and containers never go into HA history (v1 spec §7.4).
    _unrecorded_attributes = frozenset({"services", "containers"})

    def __init__(
        self, entry: HostbeaconConfigEntry, connection: HostConnection, description: HostSensorDescription
    ) -> None:
        super().__init__(entry, connection, description.key)
        self.entity_description = description

    @property
    def available(self) -> bool:
        return self._connection.online and self.entity_description.exists(self._connection)

    @property
    def native_value(self) -> StateType | datetime:
        return self.entity_description.value(self._connection)

    @property
    def extra_state_attributes(self) -> dict[str, Any] | None:
        attributes = self.entity_description.attributes
        return attributes(self._connection) if attributes else None

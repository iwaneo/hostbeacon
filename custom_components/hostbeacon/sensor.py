"""Sensors for a Host: Host status, CPU usage, Memory usage, Swap usage."""

from __future__ import annotations

from collections.abc import Callable
from dataclasses import dataclass

from homeassistant.components.sensor import (
    SensorDeviceClass,
    SensorEntity,
    SensorEntityDescription,
    SensorStateClass,
)
from homeassistant.const import PERCENTAGE
from homeassistant.core import HomeAssistant
from homeassistant.helpers.device_registry import DeviceInfo
from homeassistant.helpers.entity_platform import AddConfigEntryEntitiesCallback

from . import HostbeaconConfigEntry
from .connection import HostConnection
from .const import DOMAIN
from .protocol import System

HOST_STATUS_OPTIONS = ["online", "updating", "rebooting", "offline"]


@dataclass(frozen=True, kw_only=True)
class SystemSensorDescription(SensorEntityDescription):
    """A sensor read from the system group."""

    value: Callable[[System], float | None]


SYSTEM_SENSORS = (
    SystemSensorDescription(
        key="cpu_usage",
        translation_key="cpu_usage",
        native_unit_of_measurement=PERCENTAGE,
        state_class=SensorStateClass.MEASUREMENT,
        suggested_display_precision=0,
        value=lambda system: system.cpu_percent,
    ),
    SystemSensorDescription(
        key="memory_usage",
        translation_key="memory_usage",
        native_unit_of_measurement=PERCENTAGE,
        state_class=SensorStateClass.MEASUREMENT,
        suggested_display_precision=0,
        value=lambda system: system.memory_percent,
    ),
    SystemSensorDescription(
        key="swap_usage",
        translation_key="swap_usage",
        native_unit_of_measurement=PERCENTAGE,
        state_class=SensorStateClass.MEASUREMENT,
        suggested_display_precision=0,
        entity_registry_enabled_default=False,
        value=lambda system: system.swap_percent,
    ),
)


async def async_setup_entry(
    hass: HomeAssistant, entry: HostbeaconConfigEntry, async_add_entities: AddConfigEntryEntitiesCallback
) -> None:
    """Add the sensors of one Host."""
    connection = entry.runtime_data
    async_add_entities(
        [
            HostStatusSensor(entry, connection),
            *(SystemSensor(entry, connection, description) for description in SYSTEM_SENSORS),
        ]
    )


class HostEntity(SensorEntity):
    """An entity of one Host. Unavailable while the Host is Offline."""

    _attr_has_entity_name = True
    _attr_should_poll = False

    def __init__(self, entry: HostbeaconConfigEntry, connection: HostConnection, key: str) -> None:
        self._connection = connection
        # Entity unique IDs are <Host ID>_<entity key>.
        self._attr_unique_id = f"{entry.unique_id}_{key}"
        self._attr_device_info = DeviceInfo(identifiers={(DOMAIN, entry.unique_id)}, name=entry.title)

    async def async_added_to_hass(self) -> None:
        self.async_on_remove(self._connection.add_listener(self.async_write_ha_state))

    @property
    def available(self) -> bool:
        return self._connection.online


class HostStatusSensor(HostEntity):
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


class SystemSensor(HostEntity):
    """A value from the system group."""

    entity_description: SystemSensorDescription

    def __init__(
        self, entry: HostbeaconConfigEntry, connection: HostConnection, description: SystemSensorDescription
    ) -> None:
        super().__init__(entry, connection, description.key)
        self.entity_description = description

    @property
    def native_value(self) -> float | None:
        system = self._connection.system
        return self.entity_description.value(system) if system else None

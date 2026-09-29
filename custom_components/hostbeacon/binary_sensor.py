"""Binary sensors for a Host: the health of each physical disk (SMART)."""

from __future__ import annotations

from homeassistant.components.binary_sensor import BinarySensorDeviceClass, BinarySensorEntity
from homeassistant.core import HomeAssistant, callback
from homeassistant.helpers.entity_platform import AddConfigEntryEntitiesCallback

from . import HostbeaconConfigEntry
from .connection import HostConnection
from .entity import HostEntity
from .sensor import smart_disk


async def async_setup_entry(
    hass: HomeAssistant, entry: HostbeaconConfigEntry, async_add_entities: AddConfigEntryEntitiesCallback
) -> None:
    """Add a health sensor for each disk, and for new disks when the Host reports them."""
    connection = entry.runtime_data
    added: set[str] = set()

    @callback
    def add_new_disks() -> None:
        smart = connection.groups.smart if "smart" in connection.capabilities else None
        new = [disk.device for disk in smart.disks if disk.device not in added] if smart else []
        added.update(new)
        async_add_entities(DiskHealthSensor(entry, connection, device) for device in new)

    add_new_disks()
    entry.async_on_unload(connection.add_listener(add_new_disks))


class DiskHealthSensor(HostEntity, BinarySensorEntity):
    """On when SMART says the disk is failing."""

    _attr_device_class = BinarySensorDeviceClass.PROBLEM
    _attr_translation_key = "disk_health"

    def __init__(self, entry: HostbeaconConfigEntry, connection: HostConnection, device: str) -> None:
        super().__init__(entry, connection, f"disk_health_{device}")
        self._device = device
        self._attr_translation_placeholders = {"device": device}

    @property
    def available(self) -> bool:
        return self._connection.online and smart_disk(self._connection, self._device) is not None

    @property
    def is_on(self) -> bool | None:
        disk = smart_disk(self._connection, self._device)
        if disk is None or disk.health is None:
            return None
        return disk.health == "failing"

"""The base of every Host entity."""

from __future__ import annotations

from homeassistant.helpers.device_registry import DeviceInfo
from homeassistant.helpers.entity import Entity

from . import HostbeaconConfigEntry
from .connection import HostConnection
from .const import DOMAIN


class HostEntity(Entity):
    """An entity of one Host. Unavailable while the Host is Offline."""

    _attr_has_entity_name = True
    _attr_should_poll = False

    def __init__(self, entry: HostbeaconConfigEntry, connection: HostConnection, key: str) -> None:
        self._connection = connection
        # Entity unique IDs are <Host ID>_<entity key>.
        self._attr_unique_id = f"{entry.unique_id}_{key}"
        # The device is made and kept up to date in __init__.py.
        self._attr_device_info = DeviceInfo(identifiers={(DOMAIN, entry.unique_id)})

    async def async_added_to_hass(self) -> None:
        self.async_on_remove(self._connection.add_listener(self.async_write_ha_state))

    @property
    def available(self) -> bool:
        return self._connection.online

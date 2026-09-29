"""The base of every Host entity."""

from __future__ import annotations

from homeassistant.helpers.device_registry import DeviceInfo
from homeassistant.helpers.entity import Entity

from . import HostbeaconConfigEntry
from .connection import HostConnection
from .const import DOMAIN
from .protocol import ActionAck

# Refusals every Action shows the same way. Any other reason gets
# action_refused, unless the Action has its own message.
REFUSAL_MESSAGES = {
    "busy": "action_busy",
    "update_run_running": "action_update_run_running",
    "cannot_log": "action_cannot_log",
}


def refusal_message(ack: ActionAck, own: dict[str, str]) -> tuple[str, dict[str, str]]:
    """The translation key and placeholders for a refused Action (v1 spec §7.6).

    own maps the reasons with a message for this Action only.
    """
    reason = ack.reason or "refused"
    if reason == "duplicate":
        if ack.first_result is None:
            return "action_duplicate", {}
        if ack.first_result.result == "ok":
            return "action_duplicate_ok", {}
        return "action_duplicate_failed", {"error": ack.first_result.error or ""}
    return own.get(reason) or REFUSAL_MESSAGES.get(reason, "action_refused"), {"reason": reason}


class HostEntity(Entity):
    """An entity of one Host. Unavailable while the Host is Offline."""

    _attr_has_entity_name = True
    _attr_should_poll = False

    def __init__(self, entry: HostbeaconConfigEntry, connection: HostConnection, key: str) -> None:
        self._connection = connection
        self._entry_title = entry.title
        # Entity unique IDs are <Host ID>_<entity key>.
        self._attr_unique_id = f"{entry.unique_id}_{key}"
        # The device is made and kept up to date in __init__.py.
        self._attr_device_info = DeviceInfo(identifiers={(DOMAIN, entry.unique_id)})

    async def async_added_to_hass(self) -> None:
        self.async_on_remove(self._connection.add_listener(self.async_write_ha_state))

    @property
    def available(self) -> bool:
        return self._connection.online

    def _host_name(self) -> str:
        """The device name, as the user sees it."""
        device = self.device_entry
        return (device.name_by_user or device.name) if device and device.name else self._entry_title

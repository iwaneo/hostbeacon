"""The Reboot button of a Host (v1 spec §7.3, §7.5, §7.6)."""

from __future__ import annotations

from datetime import datetime, timedelta
from typing import NoReturn

from homeassistant.components.button import ButtonDeviceClass, ButtonEntity
from homeassistant.const import EVENT_LOGBOOK_ENTRY, EntityCategory
from homeassistant.core import Context, HomeAssistant, callback
from homeassistant.exceptions import HomeAssistantError
from homeassistant.helpers import entity_registry as er
from homeassistant.helpers.entity_platform import AddConfigEntryEntitiesCallback
from homeassistant.util import dt as dt_util

from . import HostbeaconConfigEntry
from .connection import ActionError, HostConnection
from .const import DOMAIN
from .entity import HostEntity

# The Agent refuses a Reboot this long after boot.
MIN_UPTIME = timedelta(minutes=10)

# Refusals with their own message. Any other reason gets action_refused.
REFUSAL_MESSAGES = {
    "disabled": "reboot_disabled",
    "too_soon_after_boot": "reboot_too_soon_after_boot",
    "busy": "action_busy",
    "update_run_running": "action_update_run_running",
    "cannot_log": "action_cannot_log",
}


async def async_setup_entry(
    hass: HomeAssistant, entry: HostbeaconConfigEntry, async_add_entities: AddConfigEntryEntitiesCallback
) -> None:
    """Show the Reboot button only while the Host reports that Reboot is enabled."""
    connection = entry.runtime_data
    added = False

    @callback
    def update_button() -> None:
        nonlocal added
        enabled = "reboot" in connection.enabled_actions
        if enabled and not added:
            added = True
            async_add_entities([RebootButton(entry, connection)])
        # Only a connected Agent says that Reboot is off: before it connects,
        # Home Assistant does not know.
        elif not enabled and connection.online:
            added = False
            registry = er.async_get(hass)
            if entity_id := registry.async_get_entity_id("button", DOMAIN, f"{entry.unique_id}_reboot"):
                registry.async_remove(entity_id)

    update_button()
    entry.async_on_unload(connection.add_listener(update_button))


class RebootButton(HostEntity, ButtonEntity):
    """Reboots the Host. Only admins, and calls without a user, may press it."""

    _attr_device_class = ButtonDeviceClass.RESTART
    # Keeps Alexa, Google, and "all buttons in this area" away from it.
    _attr_entity_category = EntityCategory.CONFIG
    _attr_translation_key = "reboot"

    def __init__(self, entry: HostbeaconConfigEntry, connection: HostConnection) -> None:
        super().__init__(entry, connection, "reboot")
        self._entry_title = entry.title

    @property
    def available(self) -> bool:
        return self._connection.online and "reboot" in self._connection.enabled_actions

    async def async_press(self) -> None:
        """Ask the Agent to reboot the Host."""
        # Read the caller before any await: the context may change after it.
        context = self._context
        user_id = context.user_id if context else None
        user = None
        if user_id is not None:
            user = await self.hass.auth.async_get_user(user_id)
            if user is None or not user.is_admin:
                self._refuse(context, "not_admin", "reboot_not_admin")
        try:
            # An admin without a name is still a user, not "no HA user".
            ack = await self._connection.request_action("reboot", (user.name or user.id) if user else None)
        except ActionError as err:
            raise self._error("action_no_answer") from err
        if ack.status == "accepted":
            return
        reason = ack.reason or "refused"
        if reason == "duplicate":
            if ack.first_result is None:
                self._refuse(context, reason, "action_duplicate")
            if ack.first_result.result == "ok":
                self._refuse(context, reason, "action_duplicate_ok")
            self._refuse(context, reason, "action_duplicate_failed", error=ack.first_result.error or "")
        self._refuse(context, reason, REFUSAL_MESSAGES.get(reason, "action_refused"), reason=reason)

    def _refuse(self, context: Context | None, why: str, message: str, **placeholders: str) -> NoReturn:
        """Write the logbook entry and raise the refusal message.

        HA records the press before the Integration can refuse it, so the
        logbook shows the refusal next to it.
        """
        self.hass.bus.async_fire(
            EVENT_LOGBOOK_ENTRY,
            {
                "name": "Reboot",
                "message": f"refused: {why.replace('_', ' ')}",
                "domain": DOMAIN,
                "entity_id": self.entity_id,
            },
            context=context,
        )
        raise self._error(message, **placeholders)

    def _error(self, message: str, **placeholders: str) -> HomeAssistantError:
        return HomeAssistantError(
            translation_domain=DOMAIN,
            translation_key=message,
            translation_placeholders={
                "host": self._host_name(),
                "action": "Reboot",
                "time": self._reboot_possible_after(),
                **placeholders,
            },
        )

    def _host_name(self) -> str:
        device = self.device_entry
        return (device.name_by_user or device.name) if device and device.name else self._entry_title

    def _reboot_possible_after(self) -> str:
        """The local time 10 minutes after the Host's last boot."""
        flags = self._connection.groups.flags
        boot = dt_util.parse_datetime(flags.last_boot) if flags and flags.last_boot else None
        if not isinstance(boot, datetime):
            return "a few minutes"
        return dt_util.as_local(boot + MIN_UPTIME).strftime("%H:%M")

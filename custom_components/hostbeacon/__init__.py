"""The Hostbeacon Integration: shows Linux Hosts in Home Assistant."""

from __future__ import annotations

import base64
from collections.abc import Callable
from datetime import datetime

from homeassistant.components import persistent_notification
from homeassistant.config_entries import ConfigEntry
from homeassistant.const import CONF_HOST, CONF_PORT, Platform
from homeassistant.core import HomeAssistant, callback
from homeassistant.helpers import config_validation as cv
from homeassistant.helpers import device_registry as dr
from homeassistant.helpers.aiohttp_client import async_get_clientsession
from homeassistant.helpers.event import async_track_point_in_utc_time
from homeassistant.helpers.storage import Store
from homeassistant.helpers.translation import async_get_translations
from homeassistant.helpers.typing import ConfigType
from homeassistant.util import dt as dt_util

from .connection import REBOOT_TIMEOUT, HostConnection, Reboot, remove_pairing
from .const import CONF_FINGERPRINT, CONF_KEY, DOMAIN
from .pairing import pairing_id

CONFIG_SCHEMA = cv.config_entry_only_config_schema(DOMAIN)
PLATFORMS = [Platform.BINARY_SENSOR, Platform.BUTTON, Platform.SENSOR, Platform.UPDATE]
STORAGE_VERSION = 1

type HostbeaconConfigEntry = ConfigEntry[HostConnection]


async def async_setup(hass: HomeAssistant, config: ConfigType) -> bool:
    """Set up the Hostbeacon Integration."""
    return True


async def async_setup_entry(hass: HomeAssistant, entry: HostbeaconConfigEntry) -> bool:
    """Connect to one Host and add its entities."""
    connection = HostConnection(
        async_get_clientsession(hass),
        entry.title,
        entry.data[CONF_HOST],
        entry.data[CONF_PORT],
        bytes.fromhex(entry.data[CONF_FINGERPRINT]),
        base64.b64decode(entry.data[CONF_KEY]),
    )
    entry.runtime_data = connection
    device_registry = dr.async_get(hass)
    identifiers = {(DOMAIN, entry.unique_id)}
    if device_registry.async_get_device_by_identifier((DOMAIN, entry.unique_id), entry.entry_id) is None:
        # The first name is the hostname at Pairing.
        device_registry.async_get_or_create(config_entry_id=entry.entry_id, identifiers=identifiers, name=entry.title)

    @callback
    def update_device() -> None:
        """Follow the Host's hostname, distro, Agent version, and architecture.

        A name the user gave the device is kept: it is stored apart from this one.
        """
        hello, agent = connection.hello, connection.groups.agent
        if hello is None:
            return
        distro = " ".join(part for part in (hello.distro.name, hello.distro.version) if part)
        device_registry.async_get_or_create(
            config_entry_id=entry.entry_id,
            identifiers=identifiers,
            name=agent.hostname if agent else hello.hostname,
            model=distro or None,
            sw_version=agent.agent_version if agent else hello.agent_version,
            hw_version=hello.architecture,
        )

    entry.async_on_unload(connection.add_listener(update_device))
    await _track_reboot(hass, entry, connection)
    await hass.config_entries.async_forward_entry_setups(entry, PLATFORMS)
    entry.async_create_background_task(hass, connection.run(), f"hostbeacon connection {entry.title}")
    return True


def _store(hass: HomeAssistant, entry: ConfigEntry) -> Store[dict]:
    return Store(hass, STORAGE_VERSION, f"{DOMAIN}.{entry.entry_id}")


async def _track_reboot(hass: HomeAssistant, entry: HostbeaconConfigEntry, connection: HostConnection) -> None:
    """Save the Rebooting start, so it survives a restart, and end it after 15 minutes."""
    store = _store(hass, entry)
    saved = (await store.async_load() or {}).get("reboot")
    stored = None
    if saved and (started_at := dt_util.parse_datetime(saved["started_at"])):
        stored = Reboot(started_at, saved["last_boot"])
        # Home Assistant may have been stopped for longer than the Reboot may take.
        if dt_util.utcnow() < started_at + REBOOT_TIMEOUT:
            connection.reboot = stored
    cancel_timer: Callable[[], None] | None = None

    @callback
    def reboot_timed_out(_: datetime) -> None:
        connection.end_reboot()

    @callback
    def reboot_changed() -> None:
        nonlocal stored, cancel_timer
        reboot = connection.reboot
        if cancel_timer is not None and reboot != stored:
            cancel_timer()
            cancel_timer = None
        if reboot is not None and cancel_timer is None:
            cancel_timer = async_track_point_in_utc_time(hass, reboot_timed_out, reboot.started_at + REBOOT_TIMEOUT)
        if reboot != stored:
            stored = reboot
            data = {"started_at": reboot.started_at.isoformat(), "last_boot": reboot.last_boot} if reboot else None
            store.async_delay_save(lambda: {"reboot": data}, 0)

    @callback
    def stop_timer() -> None:
        if cancel_timer is not None:
            cancel_timer()

    reboot_changed()
    entry.async_on_unload(connection.add_listener(reboot_changed))
    entry.async_on_unload(stop_timer)


async def async_unload_entry(hass: HomeAssistant, entry: HostbeaconConfigEntry) -> bool:
    """Disconnect from one Host."""
    return await hass.config_entries.async_unload_platforms(entry, PLATFORMS)


async def async_remove_entry(hass: HomeAssistant, entry: HostbeaconConfigEntry) -> None:
    """Remove this Home Assistant's Pairing on the Host.

    If the Host cannot be reached, its Pairing stays, so tell the user the
    command to run on the Host.
    """
    await _store(hass, entry).async_remove()
    key = base64.b64decode(entry.data[CONF_KEY])
    if await remove_pairing(
        async_get_clientsession(hass),
        entry.data[CONF_HOST],
        entry.data[CONF_PORT],
        bytes.fromhex(entry.data[CONF_FINGERPRINT]),
        key,
    ):
        return
    # strings.json has no category for notifications, so the texts are kept
    # with the exceptions.
    texts = await async_get_translations(hass, hass.config.language, "exceptions", [DOMAIN])
    prefix = f"component.{DOMAIN}.exceptions."
    persistent_notification.async_create(
        hass,
        texts[prefix + "pairing_not_removed.message"].format(host=entry.title, pairing_id=pairing_id(key)),
        title=texts[prefix + "pairing_not_removed_title.message"],
        notification_id=f"{DOMAIN}_pairing_not_removed_{entry.entry_id}",
    )

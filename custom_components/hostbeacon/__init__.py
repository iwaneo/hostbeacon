"""The Hostbeacon Integration: shows Linux Hosts in Home Assistant."""

from __future__ import annotations

import base64

from homeassistant.config_entries import ConfigEntry
from homeassistant.const import CONF_HOST, CONF_PORT, Platform
from homeassistant.core import HomeAssistant
from homeassistant.helpers import config_validation as cv
from homeassistant.helpers.aiohttp_client import async_get_clientsession
from homeassistant.helpers.typing import ConfigType

from .connection import HostConnection
from .const import CONF_FINGERPRINT, CONF_KEY, DOMAIN

CONFIG_SCHEMA = cv.config_entry_only_config_schema(DOMAIN)
PLATFORMS = [Platform.SENSOR]

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
    await hass.config_entries.async_forward_entry_setups(entry, PLATFORMS)
    entry.async_create_background_task(hass, connection.run(), f"hostbeacon connection {entry.title}")
    return True


async def async_unload_entry(hass: HomeAssistant, entry: HostbeaconConfigEntry) -> bool:
    """Disconnect from one Host."""
    return await hass.config_entries.async_unload_platforms(entry, PLATFORMS)

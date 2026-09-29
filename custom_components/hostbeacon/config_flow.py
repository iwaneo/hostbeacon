"""Add a Host by address or from discovery, then with a Pairing code.

Discovery is never trusted by itself: it only offers to start Pairing, and it
moves a paired Host to a new address only after the certificate pin and the
Pairing key match there.
"""

from __future__ import annotations

import base64
import logging
from typing import Any

import voluptuous as vol
from homeassistant.config_entries import SOURCE_IGNORE, ConfigEntry, ConfigEntryState, ConfigFlow, ConfigFlowResult
from homeassistant.const import CONF_HOST, CONF_PORT
from homeassistant.helpers.aiohttp_client import async_get_clientsession
from homeassistant.helpers.service_info.zeroconf import ZeroconfServiceInfo

from .connection import can_log_in
from .const import CONF_CODE, CONF_FINGERPRINT, CONF_KEY, DEFAULT_PORT, DOMAIN
from .pairing import (
    UUID,
    CannotConnect,
    InvalidCode,
    PairingFailed,
    fetch_fingerprint,
    normalize_code,
    pair,
)

_LOGGER = logging.getLogger(__name__)

USER_SCHEMA = vol.Schema(
    {
        vol.Required(CONF_HOST): str,
        vol.Required(CONF_PORT, default=DEFAULT_PORT): vol.All(vol.Coerce(int), vol.Range(min=1, max=65535)),
    }
)
PAIR_SCHEMA = vol.Schema({vol.Required(CONF_CODE): str})


class HostbeaconConfigFlow(ConfigFlow, domain=DOMAIN):
    """Add a Host."""

    VERSION = 1

    def __init__(self) -> None:
        self._host = ""
        self._port = DEFAULT_PORT

    async def async_step_user(self, user_input: dict[str, Any] | None = None) -> ConfigFlowResult:
        """Ask for the Host's address."""
        errors: dict[str, str] = {}
        if user_input is not None:
            host, port = user_input[CONF_HOST].strip(), user_input[CONF_PORT]
            try:
                await fetch_fingerprint(host, port)
            except CannotConnect:
                errors["base"] = "cannot_connect"
            else:
                self._host, self._port = host, port
                return await self.async_step_pair()
        return self.async_show_form(
            step_id="user",
            data_schema=self.add_suggested_values_to_schema(USER_SCHEMA, user_input),
            errors=errors,
        )

    async def async_step_zeroconf(self, discovery_info: ZeroconfServiceInfo) -> ConfigFlowResult:
        """Offer to pair a discovered Host, or follow a paired one to its new address."""
        instance_id = discovery_info.properties.get("id")
        if not isinstance(instance_id, str) or not UUID.fullmatch(instance_id) or not discovery_info.port:
            return self.async_abort(reason="invalid_discovery_info")
        host, port = discovery_info.host, discovery_info.port
        await self.async_set_unique_id(instance_id)
        entry = self.hass.config_entries.async_entry_for_domain_unique_id(DOMAIN, instance_id)
        if entry is not None and entry.source != SOURCE_IGNORE:
            await self._async_follow_address(entry, host, port)
        self._abort_if_unique_id_configured()

        self._host, self._port = host, port
        name = discovery_info.name.removesuffix("." + discovery_info.type)
        self.context["title_placeholders"] = {"name": name}
        return await self.async_step_pair()

    async def _async_follow_address(self, entry: ConfigEntry, host: str, port: int) -> None:
        """Move the entry to an announced address, only if the pin and key match there.

        A Host that is connected stays where it is: it may have two addresses,
        or the announcement may come from a copy of it.
        """
        if (host, port) == (entry.data[CONF_HOST], entry.data[CONF_PORT]):
            return
        if entry.state is ConfigEntryState.LOADED and entry.runtime_data.online:
            return
        if not await can_log_in(
            async_get_clientsession(self.hass),
            host,
            port,
            bytes.fromhex(entry.data[CONF_FINGERPRINT]),
            base64.b64decode(entry.data[CONF_KEY]),
        ):
            _LOGGER.debug("%s was announced at a new address, but the pin or key does not match there", entry.title)
            return
        _LOGGER.info("%s moved to a new address", entry.title)
        self.hass.config_entries.async_update_entry(entry, data={**entry.data, CONF_HOST: host, CONF_PORT: port})
        self.hass.config_entries.async_schedule_reload(entry.entry_id)

    async def async_step_reconfigure(self, user_input: dict[str, Any] | None = None) -> ConfigFlowResult:
        """Change the Host's address. Only an Agent with the pinned certificate is accepted."""
        entry = self._get_reconfigure_entry()
        errors: dict[str, str] = {}
        if user_input is not None:
            host, port = user_input[CONF_HOST].strip(), user_input[CONF_PORT]
            try:
                fingerprint = await fetch_fingerprint(host, port)
            except CannotConnect:
                errors["base"] = "cannot_connect"
            else:
                if fingerprint.hex() == entry.data[CONF_FINGERPRINT]:
                    return self.async_update_reload_and_abort(entry, data_updates={CONF_HOST: host, CONF_PORT: port})
                # Another certificate: a reinstalled Host or another machine.
                # Only Re-pair may replace the pin and key.
                errors["base"] = "certificate_changed"
        return self.async_show_form(
            step_id="reconfigure",
            data_schema=self.add_suggested_values_to_schema(USER_SCHEMA, user_input or entry.data),
            errors=errors,
            description_placeholders={"host": entry.title},
        )

    async def async_step_pair(self, user_input: dict[str, Any] | None = None) -> ConfigFlowResult:
        """Ask for the Pairing code and pair."""
        errors: dict[str, str] = {}
        if user_input is not None:
            code = normalize_code(user_input[CONF_CODE])
            try:
                if code is None:
                    raise InvalidCode
                paired = await pair(
                    async_get_clientsession(self.hass), self._host, self._port, code, self.hass.config.location_name
                )
            except InvalidCode:
                errors["base"] = "invalid_code"
            except CannotConnect:
                errors["base"] = "cannot_connect"
            except PairingFailed:
                errors["base"] = "pairing_failed"
            else:
                # Host ID = the Agent instance ID at the first Pairing.
                await self.async_set_unique_id(paired.instance_id)
                self._abort_if_unique_id_configured()
                return self.async_create_entry(
                    title=paired.hostname,
                    data={
                        CONF_HOST: self._host,
                        CONF_PORT: self._port,
                        CONF_FINGERPRINT: paired.fingerprint.hex(),
                        CONF_KEY: base64.b64encode(paired.key).decode(),
                    },
                )
        return self.async_show_form(
            step_id="pair",
            data_schema=PAIR_SCHEMA,
            errors=errors,
            description_placeholders={"host": self._host},
        )

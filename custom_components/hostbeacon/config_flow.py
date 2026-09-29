"""Add a Host by address, then with a Pairing code."""

from __future__ import annotations

import base64
from typing import Any

import voluptuous as vol
from homeassistant.config_entries import ConfigFlow, ConfigFlowResult
from homeassistant.const import CONF_HOST, CONF_PORT
from homeassistant.helpers.aiohttp_client import async_get_clientsession

from .const import CONF_CODE, CONF_FINGERPRINT, CONF_KEY, DEFAULT_PORT, DOMAIN
from .pairing import CannotConnect, InvalidCode, PairingFailed, fetch_fingerprint, normalize_code, pair

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

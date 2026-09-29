"""Add a Host by address or from discovery, then with a Pairing code.

Discovery is never trusted by itself: it only offers to start Pairing, and it
moves a paired Host to a new address only after the certificate pin and the
Pairing key match there.

Re-pair is Home Assistant's re-authentication (v1 spec §7.2). It starts from
the certificate repair or from Reconfigure, and replaces the address, pin, key,
and instance ID. The Host ID (the unique ID) and everything attached to it
stay.
"""

from __future__ import annotations

import base64
import logging
from collections.abc import Mapping
from typing import Any

import voluptuous as vol
from homeassistant.config_entries import ConfigEntry, ConfigEntryState, ConfigFlow, ConfigFlowResult
from homeassistant.const import CONF_HOST, CONF_PORT
from homeassistant.helpers import issue_registry as ir
from homeassistant.helpers.aiohttp_client import async_get_clientsession
from homeassistant.helpers.service_info.zeroconf import ZeroconfServiceInfo

from .connection import can_log_in, read_run_id, remove_pairing
from .const import (
    CONF_CODE,
    CONF_FINGERPRINT,
    CONF_INSTANCE_ID,
    CONF_KEY,
    CONF_SAME_MACHINE,
    DEFAULT_PORT,
    DOMAIN,
)
from .pairing import (
    UUID_PATTERN,
    CannotConnect,
    InvalidCode,
    Paired,
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
REPAIR_SCHEMA = USER_SCHEMA.extend({vol.Required(CONF_CODE): str})
COPY_SCHEMA = vol.Schema({vol.Required(CONF_SAME_MACHINE, default=False): bool})


class HostbeaconConfigFlow(ConfigFlow, domain=DOMAIN):
    """Add a Host."""

    VERSION = 1

    def __init__(self) -> None:
        self._host = ""
        self._port = DEFAULT_PORT
        # Re-pair: the entry, and the Pairing made for it.
        self._entry: ConfigEntry | None = None
        self._paired: Paired | None = None

    def _entry_for_instance(self, instance_id: str) -> ConfigEntry | None:
        """The entry of the Host whose Agent has this instance ID now, or had it at the first Pairing."""
        for entry in self._async_current_entries(include_ignore=False):
            if instance_id in (entry.unique_id, entry.data.get(CONF_INSTANCE_ID)):
                return entry
        return None

    async def _async_pair(self, host: str, port: int, code: str) -> tuple[Paired | None, str | None]:
        """Run the Pairing step. Returns the Pairing, or the form error."""
        normalized = normalize_code(code)
        try:
            if normalized is None:
                raise InvalidCode
            paired = await pair(async_get_clientsession(self.hass), host, port, normalized, self.hass.config.location_name)
        except InvalidCode:
            return None, "invalid_code"
        except CannotConnect:
            return None, "cannot_connect"
        except PairingFailed:
            return None, "pairing_failed"
        return paired, None

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
        if not isinstance(instance_id, str) or not UUID_PATTERN.fullmatch(instance_id) or not discovery_info.port:
            return self.async_abort(reason="invalid_discovery_info")
        host, port = discovery_info.host, discovery_info.port
        await self.async_set_unique_id(instance_id)
        entry = self._entry_for_instance(instance_id)
        if entry is not None:
            await self._async_follow_address(entry, host, port)
            return self.async_abort(reason="already_configured")
        self._abort_if_unique_id_configured()

        self._host, self._port = host, port
        name = discovery_info.name.removesuffix("." + discovery_info.type)
        self.context["title_placeholders"] = {"name": name}
        return await self.async_step_pair()

    async def _async_follow_address(self, entry: ConfigEntry, host: str, port: int) -> None:
        """Move the entry to an announced address, only if the pin and key match there.

        A Host that is connected stays where it is: it may have two addresses,
        or the announcement may come from a copy of it. If the Agent there
        passes the pin and key checks with another run ID, two machines share
        the identity, so the two-machines repair is raised (v1 spec §4.4).
        """
        if (host, port) == (entry.data[CONF_HOST], entry.data[CONF_PORT]):
            return
        session = async_get_clientsession(self.hass)
        fingerprint, key = bytes.fromhex(entry.data[CONF_FINGERPRINT]), base64.b64decode(entry.data[CONF_KEY])
        if entry.state is ConfigEntryState.LOADED and entry.runtime_data.online:
            hello = entry.runtime_data.hello
            run_id = await read_run_id(session, host, port, fingerprint, key)
            if run_id is not None and hello is not None and run_id != hello.run_id:
                _LOGGER.error("Two machines share the identity of %s", entry.title)
                ir.async_create_issue(
                    self.hass,
                    DOMAIN,
                    f"two_machines_{entry.entry_id}",
                    is_fixable=False,
                    is_persistent=False,
                    severity=ir.IssueSeverity.ERROR,
                    translation_key="two_machines",
                    translation_placeholders={"host": entry.title},
                )
            return
        if not await can_log_in(session, host, port, fingerprint, key):
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
                self._entry, self._host, self._port = entry, host, port
                return await self.async_step_reauth_confirm()
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
            paired, error = await self._async_pair(self._host, self._port, user_input[CONF_CODE])
            if paired is None:
                errors["base"] = error or "unknown"
            else:
                # Host ID = the Agent instance ID at the first Pairing. The code
                # is used up now, so a discovery flow for the same Host must not
                # stop this one.
                await self.async_set_unique_id(paired.instance_id, raise_on_progress=False)
                self._abort_if_unique_id_configured()
                if self._entry_for_instance(paired.instance_id) is not None:
                    return self.async_abort(reason="already_configured")
                return self.async_create_entry(
                    title=paired.hostname,
                    data={
                        CONF_HOST: self._host,
                        CONF_PORT: self._port,
                        CONF_FINGERPRINT: paired.fingerprint.hex(),
                        CONF_KEY: base64.b64encode(paired.key).decode(),
                        CONF_INSTANCE_ID: paired.instance_id,
                    },
                )
        return self.async_show_form(
            step_id="pair",
            data_schema=PAIR_SCHEMA,
            errors=errors,
            description_placeholders={"host": self._host},
        )

    async def async_step_reauth(self, entry_data: Mapping[str, Any]) -> ConfigFlowResult:
        """Start Re-pair from the certificate repair."""
        self._entry = self._get_reauth_entry()
        self.context["title_placeholders"] = {"name": self._entry.title}
        self._host, self._port = entry_data[CONF_HOST], entry_data[CONF_PORT]
        return await self.async_step_reauth_confirm()

    async def async_step_reauth_confirm(self, user_input: dict[str, Any] | None = None) -> ConfigFlowResult:
        """Re-pair step 1: the address and a new Pairing code."""
        assert self._entry is not None
        errors: dict[str, str] = {}
        if user_input is not None:
            host, port = user_input[CONF_HOST].strip(), user_input[CONF_PORT]
            paired, error = await self._async_pair(host, port, user_input[CONF_CODE])
            if paired is None:
                errors["base"] = error or "unknown"
            elif (other := self._entry_for_instance(paired.instance_id)) and other.entry_id != self._entry.entry_id:
                # Two entries must never share one Agent.
                errors["base"] = "other_host"
            else:
                self._host, self._port, self._paired = host, port, paired
                return await self.async_step_reauth_same_host()
        return self.async_show_form(
            step_id="reauth_confirm",
            data_schema=self.add_suggested_values_to_schema(
                REPAIR_SCHEMA, {CONF_HOST: self._host, CONF_PORT: self._port}
            ),
            errors=errors,
            description_placeholders={"host": self._entry.title},
        )

    async def async_step_reauth_same_host(self, user_input: dict[str, Any] | None = None) -> ConfigFlowResult:
        """Re-pair step 2: "Is this the same Host?" with the Agent's hostname."""
        assert self._entry is not None and self._paired is not None
        if self._paired.copied_from:
            return await self.async_step_reauth_copy()
        if user_input is not None:
            return await self._async_finish_repair()
        return self.async_show_form(
            step_id="reauth_same_host",
            data_schema=vol.Schema({}),
            description_placeholders=self._repair_placeholders(),
        )

    async def async_step_reauth_copy(self, user_input: dict[str, Any] | None = None) -> ConfigFlowResult:
        """Re-pair step 2 for an Agent that reports a copied-from list: a required tick."""
        errors: dict[str, str] = {}
        if user_input is not None:
            if user_input.get(CONF_SAME_MACHINE):
                return await self._async_finish_repair()
            errors["base"] = "same_machine_required"
        return self.async_show_form(
            step_id="reauth_copy",
            data_schema=COPY_SCHEMA,
            errors=errors,
            description_placeholders=self._repair_placeholders(),
        )

    def _repair_placeholders(self) -> dict[str, str]:
        assert self._entry is not None and self._paired is not None
        return {"host": self._entry.title, "hostname": self._paired.hostname, "address": self._host}

    async def _async_finish_repair(self) -> ConfigFlowResult:
        """Re-pair step 3: log in with the new key, remove the old Pairing, and replace the address, pin, key, and instance ID."""
        entry, paired = self._entry, self._paired
        assert entry is not None and paired is not None
        session = async_get_clientsession(self.hass)
        # The first login confirms the new key on the Agent.
        if not await can_log_in(session, self._host, self._port, paired.fingerprint, paired.key):
            return self.async_abort(reason="cannot_connect")
        # If the Agent still knows the old key, remove that Pairing. If not,
        # the Agent refuses the key and nothing happens.
        await remove_pairing(session, self._host, self._port, paired.fingerprint, base64.b64decode(entry.data[CONF_KEY]))
        # The new instance ID may wait in Discovered as a new Host.
        for flow in self._async_in_progress(include_uninitialized=True):
            if flow["context"].get("unique_id") == paired.instance_id:
                self.hass.config_entries.flow.async_abort(flow["flow_id"])
        return self.async_update_reload_and_abort(
            entry,
            data_updates={
                CONF_HOST: self._host,
                CONF_PORT: self._port,
                CONF_FINGERPRINT: paired.fingerprint.hex(),
                CONF_KEY: base64.b64encode(paired.key).decode(),
                CONF_INSTANCE_ID: paired.instance_id,
            },
            reason="reauth_successful",
        )

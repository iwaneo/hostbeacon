"""Repair flows: the fix of "Certificate of <Host> changed" starts Re-pair."""

from __future__ import annotations

from homeassistant.components.repairs import RepairsFlow
from homeassistant.core import HomeAssistant
from homeassistant.data_entry_flow import FlowResult


class CertificateRepairFlow(RepairsFlow):
    """Warn, then start Re-pair (Home Assistant re-authentication) for the Host."""

    def __init__(self, entry_id: str) -> None:
        self._entry_id = entry_id

    async def async_step_init(self, user_input: dict[str, str] | None = None) -> FlowResult:
        return await self.async_step_confirm()

    async def async_step_confirm(self, user_input: dict[str, str] | None = None) -> FlowResult:
        entry = self.hass.config_entries.async_get_entry(self._entry_id)
        if entry is None:
            return self.async_abort(reason="host_removed")
        if user_input is not None:
            entry.async_start_reauth(self.hass)
            # The repair stays until the Host is connected again.
            return self.async_abort(reason="repair_started", description_placeholders={"host": entry.title})
        return self.async_show_form(step_id="confirm", description_placeholders={"host": entry.title})


async def async_create_fix_flow(hass: HomeAssistant, issue_id: str, data: dict[str, str] | None) -> RepairsFlow:
    """Make the fix flow of a Hostbeacon repair. Only the certificate repair has one."""
    assert data is not None
    return CertificateRepairFlow(data["entry_id"])

"""The Updates entity of a Host: its Available updates (v1 spec §7.3).

Install comes with the Update run; this entity only shows the updates.
"""

from __future__ import annotations

from collections.abc import Callable
from datetime import datetime

from homeassistant.components.update import UpdateEntity, UpdateEntityFeature
from homeassistant.const import EntityCategory
from homeassistant.core import HomeAssistant
from homeassistant.helpers.entity_platform import AddConfigEntryEntitiesCallback
from homeassistant.helpers.translation import async_get_translations
from homeassistant.util import dt as dt_util

from . import HostbeaconConfigEntry
from .connection import HostConnection
from .const import DOMAIN
from .entity import HostEntity

UP_TO_DATE = "up to date"
NOT_SUPPORTED = "not supported"
type Text = Callable[..., str]

# Enough of the fingerprint to tell two sets apart in the version text.
SHORT_FINGERPRINT = 8


async def async_setup_entry(
    hass: HomeAssistant, entry: HostbeaconConfigEntry, async_add_entities: AddConfigEntryEntitiesCallback
) -> None:
    """Add the Updates entity."""
    async_add_entities([UpdatesEntity(entry, connection=entry.runtime_data)])


class UpdatesEntity(HostEntity, UpdateEntity):
    """Shows the Available updates. The version texts name the exact set, so Skip hides only that set."""

    # Keeps Alexa, Google, and "all ... in this area" away from it.
    _attr_entity_category = EntityCategory.CONFIG
    _attr_supported_features = UpdateEntityFeature.RELEASE_NOTES
    _attr_translation_key = "updates"

    def __init__(self, entry: HostbeaconConfigEntry, connection: HostConnection) -> None:
        super().__init__(entry, connection, "updates")
        self._host = entry.title

    @property
    def _supported(self) -> bool:
        return "available_updates" in self._connection.capabilities

    @property
    def installed_version(self) -> str:
        return UP_TO_DATE if self._supported else NOT_SUPPORTED

    @property
    def latest_version(self) -> str | None:
        """ "<n> updates · <short fingerprint>"; "up to date" with none, so HA shows no update."""
        if not self._supported:
            return NOT_SUPPORTED
        updates = self._connection.groups.available_updates
        if updates is None or updates.count is None:
            return None
        if updates.count == 0:
            return UP_TO_DATE
        # Without a fingerprint the text cannot name the exact set.
        if not updates.fingerprint:
            return None
        text = f"{updates.count} update" if updates.count == 1 else f"{updates.count} updates"
        return f"{text} · {updates.fingerprint[:SHORT_FINGERPRINT]}"

    def version_is_newer(self, latest_version: str, installed_version: str) -> bool:
        """An update exists exactly when the two texts differ."""
        return latest_version != installed_version

    async def async_release_notes(self) -> str | None:
        """Built when the dialog opens and never stored, because it names packages (v1 spec §7.4)."""
        texts = await async_get_translations(self.hass, self.hass.config.language, "exceptions", [DOMAIN])
        prefix = f"component.{DOMAIN}.exceptions."

        def text(key: str, **placeholders: object) -> str:
            return texts[f"{prefix}release_notes_{key}.message"].format(**placeholders)

        if not self._supported:
            return text("not_supported", host=self._host)
        groups = self._connection.groups
        parts = [_package_table(text, self._connection)]
        reboot = groups.flags.reboot_required if groups.flags else "unknown"
        parts.append(text(f"reboot_required_{reboot}"))
        parts.append(_last_run(text, self._connection))
        refreshed = groups.available_updates.last_refresh if groups.available_updates else None
        if (when := dt_util.parse_datetime(refreshed) if refreshed else None) is not None:
            parts.append(text("list_age", age=_age(when)))
        else:
            parts.append(text("list_never"))
        if "update_run" not in self._connection.enabled_actions:
            parts.append(text("update_run_off", host=self._host))
        return "\n\n".join(parts)


def _package_table(text: Text, connection: HostConnection) -> str:
    updates = connection.groups.available_updates
    if updates is None or updates.count is None:
        return text("unknown")
    if updates.count == 0:
        return text("none")
    rows = [text("table_header"), "|---|---|---|"]
    rows += [
        f"| {_cell(item.name)} | {_cell(item.installed_version)} | {_cell(item.new_version)} |"
        for item in updates.packages
    ]
    table = "\n".join(rows)
    if (more := updates.count - len(updates.packages)) > 0:
        table += "\n\n" + text("more", count=more)
    return table


def _age(when: datetime) -> str:
    """How long ago. A time a little in the future (the Host clock is ahead) is now."""
    return dt_util.get_age(min(when, dt_util.utcnow()))


def _cell(value: str) -> str:
    return value.replace("|", "\\|")


def _last_run(text: Text, connection: HostConnection) -> str:
    run = connection.groups.update_run
    if run is None:
        return text("last_run_none")
    if run.state in ("waiting_for_lock", "running", "result_unknown"):
        return text(f"last_run_{run.state}")
    if run.result is None:
        return text("last_run_none")
    finished = dt_util.parse_datetime(run.finished_at) if run.finished_at else None
    age = _age(finished) if finished else "?"
    return text(f"last_run_{run.result}", age=age, error=run.error or "")

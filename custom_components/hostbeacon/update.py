"""The update entities of a Host: Updates shows its Available updates, and
Install runs an Update run (v1 spec §7.3, §8); Agent shows the Agent version,
and Install runs an Agent update (v1 spec §10)."""

from __future__ import annotations

from collections.abc import Callable
from datetime import datetime
from typing import Any

from homeassistant.components.update import UpdateEntity, UpdateEntityFeature
from homeassistant.const import EntityCategory
from homeassistant.core import HomeAssistant
from homeassistant.exceptions import HomeAssistantError
from homeassistant.helpers.entity_platform import AddConfigEntryEntitiesCallback
from homeassistant.helpers.translation import async_get_translations
from homeassistant.util import dt as dt_util

from . import HostbeaconConfigEntry
from .connection import AGENT_UPDATE_TIMEOUT, ActionError, HostConnection
from .const import DOMAIN
from .entity import HostEntity, refusal_message

UP_TO_DATE = "up to date"
NOT_SUPPORTED = "not supported"
type Text = Callable[..., str]

# Enough of the fingerprint to tell two sets apart in the version text.
SHORT_FINGERPRINT = 8

# Refusals with a message for Update run only.
UPDATE_RUN_REFUSALS = {"disabled": "update_run_disabled"}
# Refusals with a message for Agent update only.
AGENT_UPDATE_REFUSALS = {"disabled": "agent_update_disabled"}

RELEASES_URL = "https://github.com/iwaneo/hostbeacon/releases/tag/v{version}"


async def async_setup_entry(
    hass: HomeAssistant, entry: HostbeaconConfigEntry, async_add_entities: AddConfigEntryEntitiesCallback
) -> None:
    """Add the Updates and Agent entities."""
    connection = entry.runtime_data
    async_add_entities([UpdatesEntity(entry, connection), AgentEntity(entry, connection)])


class UpdatesEntity(HostEntity, UpdateEntity):
    """Shows the Available updates. The version texts name the exact set, so Skip hides only that set.

    Install runs an Update run and waits until it ends.
    """

    # Keeps Alexa, Google, and "all ... in this area" away from it.
    _attr_entity_category = EntityCategory.CONFIG
    _attr_translation_key = "updates"

    def __init__(self, entry: HostbeaconConfigEntry, connection: HostConnection) -> None:
        super().__init__(entry, connection, "updates")
        # From the Install call until the Agent reports the run.
        self._installing = False

    @property
    def _supported(self) -> bool:
        return "available_updates" in self._connection.capabilities

    @property
    def supported_features(self) -> UpdateEntityFeature:
        """Install only when Update run is enabled on the Host and the distro has full support."""
        features = UpdateEntityFeature.RELEASE_NOTES
        if self._supported and "update_run" in self._connection.enabled_actions:
            features |= UpdateEntityFeature.INSTALL | UpdateEntityFeature.PROGRESS
        return features

    @property
    def in_progress(self) -> bool:
        """Also for a run another Home Assistant started."""
        return self._installing or self._connection.update_running

    @property
    def update_percentage(self) -> float | None:
        """apt gives a percent; dnf does not."""
        run = self._connection.groups.update_run
        return run.percent if run is not None and self._connection.update_running else None

    async def async_install(self, version: str | None, backup: bool, **kwargs: Any) -> None:
        """Run an Update run and wait until it ends. A failed run is an error."""
        # Read the caller before any await: the context may change after it.
        context = self._context
        user_id = context.user_id if context else None
        user = None
        if user_id is not None:
            # Home Assistant 2026.9 and later refuse non-admins before this.
            user = await self.hass.auth.async_get_user(user_id)
            if user is None or not user.is_admin:
                raise self._error("update_run_not_admin")
        earlier = self._connection.groups.update_run
        self._installing = True
        self.async_write_ha_state()
        try:
            try:
                # An admin without a name is still a user, not "no HA user".
                ack = await self._connection.request_action("update_run", (user.name or user.id) if user else None)
            except ActionError as err:
                raise self._error("action_no_answer") from err
            if ack.status != "accepted":
                message, placeholders = refusal_message(ack, UPDATE_RUN_REFUSALS)
                raise self._error(message, **placeholders)
            try:
                run = await self._connection.wait_for_update_run(earlier.run_id if earlier else None)
            except ActionError as err:
                raise self._error("update_run_result_unknown") from err
        finally:
            self._installing = False
        if run.state == "result_unknown":
            raise self._error("update_run_result_unknown")
        if run.result == "needs_manual_update":
            raise self._error("update_run_needs_manual_update")
        if run.result != "ok":
            raise self._error("update_run_failed", error=run.error or "")

    def _error(self, message: str, **placeholders: str) -> HomeAssistantError:
        return HomeAssistantError(
            translation_domain=DOMAIN,
            translation_key=message,
            translation_placeholders={"host": self._host_name(), "action": "Update run", **placeholders},
        )

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
            return text("not_supported", host=self._entry_title)
        groups = self._connection.groups
        parts = [_package_table(text, self._connection)]
        if (manual := _needs_manual_update(text, self._connection)) is not None:
            parts.append(manual)
        reboot = groups.flags.reboot_required if groups.flags else "unknown"
        parts.append(text(f"reboot_required_{reboot}"))
        parts.append(_last_run(text, self._connection))
        refreshed = groups.available_updates.last_refresh if groups.available_updates else None
        if (when := dt_util.parse_datetime(refreshed) if refreshed else None) is not None:
            parts.append(text("list_age", age=_age(when)))
        else:
            parts.append(text("list_never"))
        if "update_run" not in self._connection.enabled_actions:
            parts.append(text("update_run_off", host=self._entry_title))
        return "\n\n".join(parts)


class AgentEntity(HostEntity, UpdateEntity):
    """Shows the Agent version and the newest release. Install runs an Agent update.

    It stays available in limited mode, so an Agent too old for this
    Integration can still be updated.
    """

    _attr_entity_category = EntityCategory.CONFIG
    _attr_translation_key = "agent"

    def __init__(self, entry: HostbeaconConfigEntry, connection: HostConnection) -> None:
        super().__init__(entry, connection, "agent")

    @property
    def available(self) -> bool:
        return self._connection.online or self._connection.limited is not None

    @property
    def supported_features(self) -> UpdateEntityFeature:
        """Install only when Agent update is enabled on the Host."""
        features = UpdateEntityFeature.RELEASE_NOTES
        if "agent_update" in self._connection.enabled_actions:
            features |= UpdateEntityFeature.INSTALL
        return features

    @property
    def installed_version(self) -> str | None:
        return self._connection.agent_version

    @property
    def latest_version(self) -> str | None:
        """The newest release; the installed version until the Agent could check."""
        return self._connection.newest_agent_version or self.installed_version

    @property
    def release_url(self) -> str | None:
        newest = self._connection.newest_agent_version
        return RELEASES_URL.format(version=newest) if newest else None

    async def async_release_notes(self) -> str | None:
        texts = await async_get_translations(self.hass, self.hass.config.language, "exceptions", [DOMAIN])
        prefix = f"component.{DOMAIN}.exceptions."
        if "agent_update" not in self._connection.enabled_actions:
            return texts[f"{prefix}release_notes_agent_update_off.message"].format(host=self._entry_title)
        return texts[f"{prefix}release_notes_agent_update.message"].format(
            host=self._entry_title, newest=self.latest_version, installed=self.installed_version
        )

    async def async_install(self, version: str | None, backup: bool, **kwargs: Any) -> None:
        """Run an Agent update and wait for its result. A failed update is an error."""
        # Read the caller before any await: the context may change after it.
        context = self._context
        user_id = context.user_id if context else None
        user = None
        if user_id is not None:
            # Home Assistant 2026.9 and later refuse non-admins before this.
            user = await self.hass.auth.async_get_user(user_id)
            if user is None or not user.is_admin:
                raise self._error("agent_update_not_admin")
        try:
            # An admin without a name is still a user, not "no HA user".
            ack = await self._connection.request_action("agent_update", (user.name or user.id) if user else None)
        except ActionError as err:
            raise self._error("action_no_answer") from err
        if ack.status != "accepted":
            message, placeholders = refusal_message(ack, AGENT_UPDATE_REFUSALS)
            raise self._error(message, **placeholders)
        # The update restarts the Agent: the result comes on a new connection.
        try:
            result = await self._connection.wait_for_action_result(ack.action_id, AGENT_UPDATE_TIMEOUT)
        except TimeoutError as err:
            raise self._error("agent_update_result_unknown") from err
        if result.result != "ok":
            raise self._error("agent_update_failed", error=result.error or "")

    def _error(self, message: str, **placeholders: str) -> HomeAssistantError:
        return HomeAssistantError(
            translation_domain=DOMAIN,
            translation_key=message,
            translation_placeholders={"host": self._host_name(), "action": "Agent update", **placeholders},
        )


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


def _needs_manual_update(text: Text, connection: HostConnection) -> str | None:
    """The packages of the last run that stopped at its checks, until a later run passes them."""
    run = connection.last_run
    if run is None or not run.needs_manual_update.count:
        return None
    names = ", ".join(f"`{name}`" for name in run.needs_manual_update.names)
    if (more := (run.needs_manual_update.count or 0) - len(run.needs_manual_update.names)) > 0:
        names += " " + text("more", count=more)
    return text("needs_manual_update", packages=names or "-")


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

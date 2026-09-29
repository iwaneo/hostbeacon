"""Tests for the Updates entity: version texts, Skip, release notes, and names never in state (v1 spec §7.3, §7.4)."""

from __future__ import annotations

from datetime import timedelta

from homeassistant.const import STATE_OFF, STATE_ON, STATE_UNKNOWN
from homeassistant.config_entries import ConfigEntry
from homeassistant.core import HomeAssistant, State
from homeassistant.helpers import entity_registry as er
from homeassistant.util import dt as dt_util

from custom_components.hostbeacon import protocol
from custom_components.hostbeacon.const import DOMAIN

from .fake_agent import FakeAgent, flags
from .test_host import add_host, state, wait_for

FINGERPRINT = "0123456789abcdef" * 4
OTHER_FINGERPRINT = "fedcba9876543210" * 4
# Distinct names, so a test can find them anywhere in the state.
PACKAGES = [
    protocol.Package(name="libhostbeacontest1", installed_version="1.0-1", new_version="1.0-2"),
    protocol.Package(name="hostbeacon-test-tool", installed_version="2:3.1", new_version="2:3.2"),
]


def updates(
    count: int | None,
    packages: list[protocol.Package] = PACKAGES,
    fingerprint: str | None = FINGERPRINT,
    last_refresh: str | None = None,
) -> protocol.AvailableUpdates:
    return protocol.AvailableUpdates(
        count=count, packages=packages if count else [], fingerprint=fingerprint, last_refresh=last_refresh
    )


def updates_entity_id(hass: HomeAssistant, entry: ConfigEntry) -> str:
    found = er.async_get(hass).async_get_entity_id("update", DOMAIN, f"{entry.unique_id}_updates")
    assert found
    return found


def updates_state(hass: HomeAssistant, entry: ConfigEntry) -> State:
    return hass.states.get(updates_entity_id(hass, entry))


async def add_updates_host(hass: HomeAssistant, agent: FakeAgent, group: protocol.AvailableUpdates) -> ConfigEntry:
    agent.capabilities = ["available_updates"]
    agent.groups = protocol.Groups(flags=flags(), available_updates=group)
    return await add_host(hass, agent)


async def release_notes(hass: HomeAssistant, hass_ws_client, entry: ConfigEntry) -> str:
    client = await hass_ws_client(hass)
    await client.send_json_auto_id({"type": "update/release_notes", "entity_id": updates_entity_id(hass, entry)})
    result = await client.receive_json()
    assert result["success"], result
    return result["result"]


async def test_updates_show_count_and_short_fingerprint(hass: HomeAssistant, agent: FakeAgent) -> None:
    entry = await add_updates_host(hass, agent, updates(2))
    current = updates_state(hass, entry)
    assert current.state == STATE_ON
    assert current.attributes["installed_version"] == "up to date"
    assert current.attributes["latest_version"] == "2 updates · 01234567"
    assert current.attributes["friendly_name"] == "test-host Updates"
    registry_entry = er.async_get(hass).async_get(current.entity_id)
    assert registry_entry.entity_category == "config"
    # Install is not offered yet: only release notes.
    assert current.attributes["supported_features"] == 16

    await agent.send_groups(protocol.Groups(available_updates=updates(1, PACKAGES[:1])))
    await wait_for(lambda: updates_state(hass, entry).attributes["latest_version"] == "1 update · 01234567")


async def test_zero_updates_shows_no_update(hass: HomeAssistant, agent: FakeAgent) -> None:
    entry = await add_updates_host(hass, agent, updates(0))
    current = updates_state(hass, entry)
    assert current.state == STATE_OFF
    assert current.attributes["latest_version"] == "up to date"
    assert current.attributes["installed_version"] == "up to date"


async def test_unknown_updates(hass: HomeAssistant, agent: FakeAgent) -> None:
    """The Host cannot read its package cache right now."""
    entry = await add_updates_host(hass, agent, updates(None, fingerprint=None))
    assert updates_state(hass, entry).state == STATE_UNKNOWN


async def test_skip_hides_only_that_set(hass: HomeAssistant, agent: FakeAgent) -> None:
    entry = await add_updates_host(hass, agent, updates(2))
    entity = updates_entity_id(hass, entry)
    await hass.services.async_call("update", "skip", {"entity_id": entity}, blocking=True)
    assert updates_state(hass, entry).state == STATE_OFF
    assert updates_state(hass, entry).attributes["skipped_version"] == "2 updates · 01234567"

    # The same set again stays hidden.
    await agent.send_groups(protocol.Groups(available_updates=updates(2, last_refresh="2026-09-29T03:00:00Z")))
    await wait_for(lambda: agent_sent_last_refresh(hass, entry))
    assert updates_state(hass, entry).state == STATE_OFF

    # A new set with the same count shows again.
    await agent.send_groups(protocol.Groups(available_updates=updates(2, fingerprint=OTHER_FINGERPRINT)))
    await wait_for(lambda: updates_state(hass, entry).state == STATE_ON)
    assert updates_state(hass, entry).attributes["latest_version"] == "2 updates · fedcba98"


def agent_sent_last_refresh(hass: HomeAssistant, entry: ConfigEntry) -> bool:
    connection = entry.runtime_data
    return bool(connection.groups.available_updates and connection.groups.available_updates.last_refresh)


async def test_release_notes(hass: HomeAssistant, agent: FakeAgent, hass_ws_client) -> None:
    refreshed = (dt_util.utcnow() - timedelta(hours=3)).strftime("%Y-%m-%dT%H:%M:%SZ")
    agent.capabilities = ["available_updates"]
    agent.groups = protocol.Groups(flags=flags("yes"), available_updates=updates(5, last_refresh=refreshed))
    entry = await add_host(hass, agent)

    notes = await release_notes(hass, hass_ws_client, entry)
    assert "| Package | Installed | New |" in notes
    assert "| libhostbeacontest1 | 1.0-1 | 1.0-2 |" in notes
    assert "| hostbeacon-test-tool | 2:3.1 | 2:3.2 |" in notes
    # The Agent sends a capped list: the rest is a count.
    assert "+3 more" in notes
    assert "**Reboot required:** Yes" in notes
    assert "**Last Update run:** None yet" in notes
    assert "**Package list refreshed:** 3 hours ago" in notes
    assert "Update run is turned off on test-host" in notes


async def test_release_notes_without_updates_or_refresh(
    hass: HomeAssistant, agent: FakeAgent, hass_ws_client
) -> None:
    agent.enabled_actions = ["update_run"]
    entry = await add_updates_host(hass, agent, updates(0))
    notes = await release_notes(hass, hass_ws_client, entry)
    assert "No Available updates." in notes
    assert "+" not in notes
    assert "**Reboot required:** No" in notes
    assert "**Package list refreshed:** Not yet by Hostbeacon" in notes
    assert "turned off" not in notes


async def test_last_update_run_in_release_notes(hass: HomeAssistant, agent: FakeAgent, hass_ws_client) -> None:
    entry = await add_updates_host(hass, agent, updates(0))
    finished = (dt_util.utcnow() - timedelta(days=2)).strftime("%Y-%m-%dT%H:%M:%SZ")
    run = protocol.UpdateRun(
        run_id="d4c3b2a1-9f8e-4d7c-b6a5-493827160a5b",
        state="finished",
        percent=None,
        started_at=finished,
        finished_at=finished,
        result="failed",
        installed=3,
        remaining=2,
        error="dpkg was interrupted",
        needs_manual_update=protocol.NameList(count=0, names=[]),
    )
    await agent.send_groups(protocol.Groups(update_run=run))
    await wait_for(lambda: entry.runtime_data.groups.update_run.result == "failed")
    notes = await release_notes(hass, hass_ws_client, entry)
    assert "**Last Update run:** Failed, 2 days ago. dpkg was interrupted" in notes


async def test_package_names_are_never_in_state(hass: HomeAssistant, agent: FakeAgent, hass_ws_client) -> None:
    """Names appear only in the release notes, which HA never stores (v1 spec §7.4)."""
    enabled = er.async_get(hass)
    enabled.async_get_or_create("sensor", DOMAIN, f"{agent.instance_id}_package_list_refreshed")
    entry = await add_updates_host(hass, agent, updates(2, last_refresh="2026-09-29T03:00:00Z"))
    assert "libhostbeacontest1" in await release_notes(hass, hass_ws_client, entry)

    for current in hass.states.async_all():
        text = f"{current.state} {current.attributes}"
        for package in PACKAGES:
            assert package.name not in text, current.entity_id
            assert package.new_version not in text, current.entity_id


async def test_not_supported_on_partial_support_distros(
    hass: HomeAssistant, agent: FakeAgent, hass_ws_client
) -> None:
    agent.capabilities = []
    entry = await add_host(hass, agent)
    current = updates_state(hass, entry)
    assert current.state == STATE_OFF
    assert current.attributes["installed_version"] == "not supported"
    assert current.attributes["latest_version"] == "not supported"
    notes = await release_notes(hass, hass_ws_client, entry)
    assert notes == "Available updates are not supported on test-host."
    assert state(hass, entry, "available_updates") is None


async def test_package_list_refreshed_sensor(hass: HomeAssistant, agent: FakeAgent) -> None:
    er.async_get(hass).async_get_or_create("sensor", DOMAIN, f"{agent.instance_id}_package_list_refreshed")
    entry = await add_updates_host(hass, agent, updates(0, last_refresh="2026-09-29T03:00:00Z"))
    assert state(hass, entry, "package_list_refreshed") == "2026-09-29T03:00:00+00:00"
    await agent.send_groups(protocol.Groups(available_updates=updates(0)))
    await wait_for(lambda: state(hass, entry, "package_list_refreshed") == STATE_UNKNOWN)

"""Tests for the Reboot button: who may press it, refusals, and Host status Rebooting."""

from __future__ import annotations

from datetime import timedelta
from typing import Any
from unittest.mock import patch

import pytest
from homeassistant.components.button import SERVICE_PRESS
from homeassistant.config_entries import ConfigEntry
from homeassistant.const import ATTR_ENTITY_ID, EVENT_LOGBOOK_ENTRY, STATE_UNAVAILABLE
from homeassistant.core import Context, Event, HomeAssistant
from homeassistant.exceptions import HomeAssistantError
from homeassistant.helpers import entity_registry as er
from homeassistant.util import dt as dt_util
from pytest_homeassistant_custom_component.common import MockUser, async_fire_time_changed

from custom_components.hostbeacon import protocol
from custom_components.hostbeacon.const import DOMAIN

from .fake_agent import FakeAgent, flags
from .test_host import add_host, state, wait_for


def reboot_button(hass: HomeAssistant, entry: ConfigEntry) -> str | None:
    return er.async_get(hass).async_get_entity_id("button", DOMAIN, f"{entry.unique_id}_reboot")


async def add_rebootable_host(hass: HomeAssistant, agent: FakeAgent) -> tuple[ConfigEntry, str]:
    agent.enabled_actions = ["reboot"]
    entry = await add_host(hass, agent)
    button = reboot_button(hass, entry)
    assert button
    return entry, button


async def press(hass: HomeAssistant, button: str, user_id: str | None = None) -> None:
    await hass.services.async_call(
        "button", SERVICE_PRESS, {ATTR_ENTITY_ID: button}, blocking=True, context=Context(user_id=user_id)
    )


def logbook_entries(hass: HomeAssistant) -> list[Event]:
    entries: list[Event] = []
    hass.bus.async_listen(EVENT_LOGBOOK_ENTRY, entries.append)
    return entries


async def test_no_button_when_reboot_is_not_enabled(hass: HomeAssistant, agent: FakeAgent) -> None:
    entry = await add_host(hass, agent)
    assert reboot_button(hass, entry) is None


async def test_button_goes_when_reboot_is_turned_off_and_comes_back(hass: HomeAssistant, agent: FakeAgent) -> None:
    entry, _ = await add_rebootable_host(hass, agent)

    agent.enabled_actions = []
    await agent.send_groups(protocol.Groups(agent=agent._agent_info()))
    await wait_for(lambda: reboot_button(hass, entry) is None)

    agent.enabled_actions = ["reboot"]
    await agent.send_groups(protocol.Groups(agent=agent._agent_info()))
    await wait_for(lambda: reboot_button(hass, entry) is not None)
    await hass.async_block_till_done()
    assert hass.states.get(reboot_button(hass, entry)).state != STATE_UNAVAILABLE


async def test_button_stays_while_home_assistant_waits_for_the_agent(
    hass: HomeAssistant, agent: FakeAgent
) -> None:
    """After a restart, Home Assistant does not know the enabled Actions yet."""
    entry, button = await add_rebootable_host(hass, agent)
    await agent.stop()
    assert await hass.config_entries.async_unload(entry.entry_id)
    assert await hass.config_entries.async_setup(entry.entry_id)
    await hass.async_block_till_done()

    assert reboot_button(hass, entry) == button


async def test_button_is_a_config_restart_button(hass: HomeAssistant, agent: FakeAgent) -> None:
    _, button = await add_rebootable_host(hass, agent)
    registry_entry = er.async_get(hass).async_get(button)
    assert registry_entry.entity_category == "config"
    assert registry_entry.original_device_class == "restart"
    assert hass.states.get(button).attributes["friendly_name"] == "test-host Reboot"


async def test_admin_reboots_and_host_status_is_rebooting(
    hass: HomeAssistant, agent: FakeAgent, hass_admin_user: MockUser
) -> None:
    entry, button = await add_rebootable_host(hass, agent)

    await press(hass, button, hass_admin_user.id)

    assert len(agent.action_requests) == 1
    request = agent.action_requests[0]
    assert request.action == "reboot"
    assert request.user == hass_admin_user.name
    assert state(hass, entry, "host_status") == "rebooting"


async def test_call_without_a_user_is_allowed(hass: HomeAssistant, agent: FakeAgent) -> None:
    """Automations, voice satellites, and HomeKit call without a user."""
    entry, button = await add_rebootable_host(hass, agent)

    await press(hass, button)

    assert [request.user for request in agent.action_requests] == [None]
    assert state(hass, entry, "host_status") == "rebooting"


async def test_non_admin_is_refused(hass: HomeAssistant, agent: FakeAgent, hass_owner_user: MockUser) -> None:
    """A user who may control entities, but is not an admin."""
    entry, button = await add_rebootable_host(hass, agent)
    user = await hass.auth.async_create_user("Guest", group_ids=["system-users"])
    assert not user.is_admin
    logbook = logbook_entries(hass)

    with pytest.raises(HomeAssistantError) as refused:
        await press(hass, button, user.id)

    assert refused.value.translation_key == "reboot_not_admin"
    assert str(refused.value) == "Only Home Assistant admins can reboot test-host"
    assert agent.action_requests == []
    await hass.async_block_till_done()
    assert [(event.data["name"], event.data["message"], event.data["entity_id"]) for event in logbook] == [
        ("Reboot", "refused: not admin", button)
    ]
    assert state(hass, entry, "host_status") == "online"


@pytest.mark.parametrize(
    ("reason", "first_result", "message"),
    [
        ("disabled", None, "Reboot is turned off on test-host. The Host owner can turn it on with `sudo hostbeacon setup`."),
        ("busy", None, "test-host is busy with a package task. Try again later."),
        ("update_run_running", None, "An Update run is already running on test-host."),
        ("cannot_log", None, "test-host refused: the Agent cannot write its Action log."),
        ("duplicate", None, "This Reboot was already sent to test-host. Waiting for its result."),
        (
            "duplicate",
            protocol.ActionOutcome(result="ok", error=None),
            "This Reboot was already sent to test-host. Its result: OK.",
        ),
        (
            "duplicate",
            protocol.ActionOutcome(result="failed", error="refused: busy"),
            "This Reboot was already sent to test-host. Its result: failed (refused: busy).",
        ),
    ],
)
async def test_refusal_shows_its_message_and_a_logbook_entry(
    hass: HomeAssistant,
    agent: FakeAgent,
    hass_admin_user: MockUser,
    reason: str,
    first_result: protocol.ActionOutcome | None,
    message: str,
) -> None:
    entry, button = await add_rebootable_host(hass, agent)
    agent.refusal = (reason, first_result)
    logbook = logbook_entries(hass)

    with pytest.raises(HomeAssistantError) as refused:
        await press(hass, button, hass_admin_user.id)

    # Home Assistant drops the final period.
    assert str(refused.value) == message.removesuffix(".")
    await hass.async_block_till_done()
    assert [(event.data["name"], event.data["message"]) for event in logbook] == [
        ("Reboot", f"refused: {reason.replace('_', ' ')}")
    ]
    assert state(hass, entry, "host_status") == "online"


async def test_too_soon_after_boot_says_when_reboot_is_possible(
    hass: HomeAssistant, agent: FakeAgent, hass_admin_user: MockUser
) -> None:
    await hass.config.async_set_time_zone("Asia/Jerusalem")
    agent.groups = protocol.Groups(flags=flags(last_boot="2026-09-29T10:55:00Z"))
    _, button = await add_rebootable_host(hass, agent)
    agent.refusal = ("too_soon_after_boot", None)

    with pytest.raises(HomeAssistantError) as refused:
        await press(hass, button, hass_admin_user.id)

    assert str(refused.value) == "test-host started less than 10 minutes ago. Reboot is possible after 14:05"


async def test_no_answer_is_an_error(hass: HomeAssistant, agent: FakeAgent, hass_admin_user: MockUser) -> None:
    entry, button = await add_rebootable_host(hass, agent)
    agent.answer_actions = False

    with patch("custom_components.hostbeacon.connection.ACK_TIMEOUT", 0.1), pytest.raises(HomeAssistantError) as failed:
        await press(hass, button, hass_admin_user.id)

    assert failed.value.translation_key == "action_no_answer"
    assert state(hass, entry, "host_status") == "online"


async def test_rebooting_until_the_agent_is_back_with_a_new_boot_time(
    hass: HomeAssistant, agent: FakeAgent, hass_admin_user: MockUser
) -> None:
    entry, button = await add_rebootable_host(hass, agent)
    await press(hass, button, hass_admin_user.id)

    await agent.stop()
    await wait_for(lambda: hass.states.get(button).state == STATE_UNAVAILABLE)
    assert state(hass, entry, "host_status") == "rebooting"

    # Back with the same boot time: the Host has not rebooted yet.
    await agent.start()
    await wait_for(lambda: hass.states.get(button).state != STATE_UNAVAILABLE)
    assert state(hass, entry, "host_status") == "rebooting"

    await agent.stop()
    agent.groups = protocol.Groups(flags=flags(last_boot="2026-09-29T12:00:00Z"))
    await agent.start()
    await wait_for(lambda: state(hass, entry, "host_status") == "online")


async def test_failed_reboot_ends_rebooting(hass: HomeAssistant, agent: FakeAgent, hass_admin_user: MockUser) -> None:
    entry, button = await add_rebootable_host(hass, agent)
    await press(hass, button, hass_admin_user.id)

    await agent.send_action_result("failed", "systemctl reboot exited with status 1")

    await wait_for(lambda: state(hass, entry, "host_status") == "online")


async def test_reboot_that_fails_at_once_does_not_show_rebooting(
    hass: HomeAssistant, agent: FakeAgent, hass_admin_user: MockUser
) -> None:
    """The failed result comes in the same moment as the ack."""
    entry, button = await add_rebootable_host(hass, agent)
    agent.result_at_once = protocol.ActionOutcome(result="failed", error="systemctl reboot exited with status 1")

    await press(hass, button, hass_admin_user.id)
    await hass.async_block_till_done()

    assert state(hass, entry, "host_status") == "online"


async def test_rebooting_survives_a_home_assistant_restart(
    hass: HomeAssistant, agent: FakeAgent, hass_admin_user: MockUser
) -> None:
    entry, button = await add_rebootable_host(hass, agent)
    await press(hass, button, hass_admin_user.id)
    await agent.stop()

    assert await hass.config_entries.async_unload(entry.entry_id)
    assert await hass.config_entries.async_setup(entry.entry_id)
    await hass.async_block_till_done()

    assert state(hass, entry, "host_status") == "rebooting"


async def test_offline_after_15_minutes_without_the_agent(
    hass: HomeAssistant, agent: FakeAgent, hass_admin_user: MockUser
) -> None:
    entry, button = await add_rebootable_host(hass, agent)
    pressed = dt_util.utcnow()
    await press(hass, button, hass_admin_user.id)
    await agent.stop()
    await wait_for(lambda: hass.states.get(button).state == STATE_UNAVAILABLE)

    async_fire_time_changed(hass, pressed + timedelta(minutes=14))
    await hass.async_block_till_done()
    assert state(hass, entry, "host_status") == "rebooting"

    async_fire_time_changed(hass, pressed + timedelta(minutes=15, seconds=1))
    await hass.async_block_till_done()
    assert state(hass, entry, "host_status") == "offline"

    # The Rebooting mark is gone, also after a restart.
    assert await hass.config_entries.async_unload(entry.entry_id)
    assert await hass.config_entries.async_setup(entry.entry_id)
    await hass.async_block_till_done()
    assert state(hass, entry, "host_status") == "offline"


async def test_rebooting_mark_older_than_15_minutes_is_offline_after_a_restart(
    hass: HomeAssistant, agent: FakeAgent, hass_admin_user: MockUser, hass_storage: dict[str, Any]
) -> None:
    """Home Assistant was down longer than the Reboot may take."""
    entry, button = await add_rebootable_host(hass, agent)
    await press(hass, button, hass_admin_user.id)
    await agent.stop()
    assert await hass.config_entries.async_unload(entry.entry_id)
    await hass.async_block_till_done()

    saved = hass_storage[f"{DOMAIN}.{entry.entry_id}"]["data"]["reboot"]
    saved["started_at"] = (dt_util.utcnow() - timedelta(minutes=16)).isoformat()
    assert await hass.config_entries.async_setup(entry.entry_id)
    await hass.async_block_till_done()

    assert state(hass, entry, "host_status") == "offline"

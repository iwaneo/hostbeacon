"""Tests for the Agent entity (Agent update) and limited mode (v1 spec §6.5, §7.3, §7.7, §10)."""

from __future__ import annotations

import asyncio
import uuid
from typing import Any

import pytest
from homeassistant.components.update import DOMAIN as UPDATE_DOMAIN, SERVICE_INSTALL
from homeassistant.config_entries import ConfigEntry
from homeassistant.const import ATTR_ENTITY_ID, STATE_OFF, STATE_ON, STATE_UNAVAILABLE, EntityCategory
from homeassistant.core import Context, HomeAssistant, State
from homeassistant.exceptions import HomeAssistantError, Unauthorized
from homeassistant.helpers import entity_registry as er
from homeassistant.helpers import issue_registry as ir
from pytest_homeassistant_custom_component.common import MockUser

from custom_components.hostbeacon import protocol
from custom_components.hostbeacon.const import DOMAIN

from .fake_agent import FakeAgent
from .test_host import add_host, entity_id, state, wait_for


def agent_entity_id(hass: HomeAssistant, entry: ConfigEntry) -> str:
    found = er.async_get(hass).async_get_entity_id("update", DOMAIN, f"{entry.unique_id}_agent")
    assert found
    return found


def agent_state(hass: HomeAssistant, entry: ConfigEntry) -> State:
    return hass.states.get(agent_entity_id(hass, entry))


async def add_agent_host(hass: HomeAssistant, agent: FakeAgent, enabled: bool = True) -> ConfigEntry:
    agent.agent_version = "0.1.0"
    agent.newest_agent_version = "0.2.0"
    agent.enabled_actions = ["agent_update"] if enabled else []
    return await add_host(hass, agent)


def install(hass: HomeAssistant, entry: ConfigEntry, user_id: str | None = None) -> asyncio.Task[Any]:
    """Start Install; the call waits until the Agent update ends."""
    return hass.async_create_task(
        hass.services.async_call(
            UPDATE_DOMAIN,
            SERVICE_INSTALL,
            {ATTR_ENTITY_ID: agent_entity_id(hass, entry)},
            blocking=True,
            context=Context(user_id=user_id),
        )
    )


def result(request: protocol.ActionRequest, outcome: str, error: str | None = None) -> protocol.ActionResult:
    return protocol.ActionResult(
        id=str(uuid.uuid4()), action_id=request.action_id, action="agent_update", result=outcome, error=error
    )


async def release_notes(hass: HomeAssistant, hass_ws_client, entry: ConfigEntry) -> str:
    client = await hass_ws_client(hass)
    await client.send_json_auto_id({"type": "update/release_notes", "entity_id": agent_entity_id(hass, entry)})
    answer = await client.receive_json()
    assert answer["success"], answer
    return answer["result"]


def issue(hass: HomeAssistant, kind: str, entry: ConfigEntry) -> ir.IssueEntry | None:
    return ir.async_get(hass).async_get_issue(DOMAIN, f"{kind}_{entry.entry_id}")


async def test_agent_entity_shows_the_installed_and_newest_version(hass: HomeAssistant, agent: FakeAgent) -> None:
    entry = await add_agent_host(hass, agent)
    current = agent_state(hass, entry)
    assert current.state == STATE_ON
    assert current.attributes["installed_version"] == "0.1.0"
    assert current.attributes["latest_version"] == "0.2.0"
    assert current.attributes["release_url"] == "https://github.com/iwaneo/hostbeacon/releases/tag/v0.2.0"
    registry_entry = er.async_get(hass).async_get(agent_entity_id(hass, entry))
    assert registry_entry.entity_category is EntityCategory.CONFIG

    # Before the Agent could check, or when it is the newest: no update.
    agent.newest_agent_version = None
    await agent.send_groups(protocol.Groups(agent=agent._agent_info()))
    await wait_for(lambda: agent_state(hass, entry).state == STATE_OFF)
    assert agent_state(hass, entry).attributes["latest_version"] == "0.1.0"


async def test_install_is_offered_only_when_agent_update_is_enabled(
    hass: HomeAssistant, agent: FakeAgent, hass_ws_client
) -> None:
    entry = await add_agent_host(hass, agent, enabled=False)
    # Release notes only.
    assert agent_state(hass, entry).attributes["supported_features"] == 16
    notes = await release_notes(hass, hass_ws_client, entry)
    assert "Agent update is turned off on test-host" in notes
    assert "`sudo hostbeacon update`" in notes

    agent.enabled_actions = ["agent_update"]
    await agent.send_groups(protocol.Groups(agent=agent._agent_info()))
    await wait_for(lambda: agent_state(hass, entry).attributes["supported_features"] == 1 | 16)
    notes = await release_notes(hass, hass_ws_client, entry)
    assert "0.2.0" in notes and "signature" in notes


async def test_install_updates_the_agent_and_waits_for_it_to_come_back(
    hass: HomeAssistant, agent: FakeAgent, hass_admin_user: MockUser
) -> None:
    entry = await add_agent_host(hass, agent)
    task = install(hass, entry, hass_admin_user.id)
    await wait_for(lambda: len(agent.action_requests) == 1)
    request = agent.action_requests[0]
    assert (request.action, request.user) == ("agent_update", hass_admin_user.name)
    await wait_for(lambda: agent_state(hass, entry).attributes["in_progress"] is True)

    # The update restarts the Agent. The new version sends the result after
    # its snapshot.
    await agent.stop()
    agent.agent_version = "0.2.0"
    agent.results_on_connect = [result(request, "ok")]
    await agent.start()
    await asyncio.wait_for(task, 5)
    await wait_for(lambda: agent_state(hass, entry).attributes["installed_version"] == "0.2.0")
    assert agent_state(hass, entry).state == STATE_OFF


async def test_failed_agent_update_is_an_error(
    hass: HomeAssistant, agent: FakeAgent, hass_admin_user: MockUser
) -> None:
    entry = await add_agent_host(hass, agent)
    task = install(hass, entry, hass_admin_user.id)
    await wait_for(lambda: len(agent.action_requests) == 1)
    error = "Version 0.2.0 did not pass the health check, so the Agent went back to 0.1.0"
    await agent.send_action_result("failed", error)
    with pytest.raises(HomeAssistantError, match="The Agent update on test-host failed: Version 0.2.0 did not pass"):
        await asyncio.wait_for(task, 5)


async def test_refused_agent_update_says_why(hass: HomeAssistant, agent: FakeAgent) -> None:
    entry = await add_agent_host(hass, agent)
    agent.refusal = ("disabled", None)
    with pytest.raises(HomeAssistantError, match="Agent update is turned off on test-host"):
        await asyncio.wait_for(install(hass, entry), 5)
    agent.refusal = ("busy", None)
    with pytest.raises(HomeAssistantError, match="test-host is busy with a package task"):
        await asyncio.wait_for(install(hass, entry), 5)


async def test_only_admins_can_update_the_agent(
    hass: HomeAssistant, agent: FakeAgent, hass_owner_user: MockUser
) -> None:
    entry = await add_agent_host(hass, agent)
    user = await hass.auth.async_create_user("Guest", group_ids=["system-users"])
    with pytest.raises(Unauthorized):
        await asyncio.wait_for(install(hass, entry, user.id), 5)
    # Older versions of Home Assistant do not check, so the Integration does.
    entity = hass.data["entity_components"][UPDATE_DOMAIN].get_entity(agent_entity_id(hass, entry))
    entity.async_set_context(Context(user_id=user.id))
    with pytest.raises(HomeAssistantError, match="Only Home Assistant admins can update the Agent on test-host"):
        await entity.async_install(None, False)
    assert agent.action_requests == []


@pytest.mark.parametrize(
    ("agent_majors", "repair"),
    # An Agent with only a newer major needs a newer Integration, and one with
    # only an older major needs a newer Agent.
    [([2], "update_integration"), ([0], "update_agent")],
)
async def test_limited_mode_is_offline_with_a_repair_and_agent_update_still_works(
    hass: HomeAssistant,
    agent: FakeAgent,
    hass_admin_user: MockUser,
    agent_majors: list[int],
    repair: str,
) -> None:
    entry = await add_agent_host(hass, agent)
    await agent.stop()
    agent.protocol_majors = agent_majors
    await agent.start()

    await wait_for(lambda: issue(hass, repair, entry) is not None)
    found = issue(hass, repair, entry)
    assert found.severity is ir.IssueSeverity.ERROR
    assert found.translation_placeholders["host"] == "test-host"
    assert state(hass, entry, "host_status") == "offline"
    assert hass.states.get(entity_id(hass, entry, "cpu_usage")).state == STATE_UNAVAILABLE
    # The Agent entity stays available.
    await wait_for(lambda: agent_state(hass, entry).state == STATE_ON)

    task = install(hass, entry, hass_admin_user.id)
    await wait_for(lambda: len(agent.action_requests) == 1)
    request = agent.action_requests[0]
    await agent.stop()
    agent.protocol_majors = protocol.PROTOCOL_MAJORS
    agent.agent_version = "0.2.0"
    agent.results_on_connect = [result(request, "ok")]
    await agent.start()
    await asyncio.wait_for(task, 5)

    await wait_for(lambda: state(hass, entry, "host_status") == "online")
    assert issue(hass, repair, entry) is None


async def test_a_result_right_after_the_ack_is_not_lost(
    hass: HomeAssistant, agent: FakeAgent, hass_admin_user: MockUser
) -> None:
    """An update that stops at once (for example, the download fails) reports right after the ack."""
    entry = await add_agent_host(hass, agent)
    agent.result_at_once = protocol.ActionOutcome(result="failed", error="Cannot check the newest release: 404 Not Found")
    with pytest.raises(HomeAssistantError, match="Cannot check the newest release"):
        await asyncio.wait_for(install(hass, entry, hass_admin_user.id), 5)

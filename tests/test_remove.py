"""Tests for removing a Host in Home Assistant."""

from __future__ import annotations

import base64

from homeassistant.core import HomeAssistant
from homeassistant.setup import async_setup_component

from custom_components.hostbeacon.const import CONF_KEY
from custom_components.hostbeacon.pairing import pairing_id

from .fake_agent import FakeAgent
from .test_host import add_host, wait_for


async def notifications(hass: HomeAssistant, hass_ws_client) -> dict[str, dict]:
    client = await hass_ws_client(hass)
    await client.send_json_auto_id({"type": "persistent_notification/get"})
    response = await client.receive_json()
    assert response["success"]
    return {item["notification_id"]: item for item in response["result"]}


async def test_removing_the_host_removes_its_pairing_on_the_host(
    hass: HomeAssistant, agent: FakeAgent, hass_ws_client
) -> None:
    assert await async_setup_component(hass, "persistent_notification", {})
    entry = await add_host(hass, agent)
    key = base64.b64decode(entry.data[CONF_KEY])
    assert key in agent.keys

    await hass.config_entries.async_remove(entry.entry_id)
    await hass.async_block_till_done()

    assert key not in agent.keys
    await wait_for(lambda: agent.connected == 0)
    assert await notifications(hass, hass_ws_client) == {}


async def test_unreachable_host_shows_a_notification_with_the_command(
    hass: HomeAssistant, agent: FakeAgent, hass_ws_client
) -> None:
    assert await async_setup_component(hass, "persistent_notification", {})
    entry = await add_host(hass, agent)
    key = base64.b64decode(entry.data[CONF_KEY])
    await agent.stop()

    await hass.config_entries.async_remove(entry.entry_id)
    await hass.async_block_till_done()

    [notification] = (await notifications(hass, hass_ws_client)).values()
    assert notification["title"] == "Host removed, but its Pairing is still on the Host"
    assert "test-host" in notification["message"]
    assert f"sudo hostbeacon pairings remove {pairing_id(key)}" in notification["message"]
    assert entry.data[CONF_KEY] not in notification["message"]


async def test_host_that_already_forgot_the_key_shows_no_notification(
    hass: HomeAssistant, agent: FakeAgent, hass_ws_client
) -> None:
    """The owner already ran `hostbeacon pairings remove`: nothing is left to do."""
    assert await async_setup_component(hass, "persistent_notification", {})
    entry = await add_host(hass, agent)
    agent.keys.clear()

    await hass.config_entries.async_remove(entry.entry_id)
    await hass.async_block_till_done()

    assert await notifications(hass, hass_ws_client) == {}

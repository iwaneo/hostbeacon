"""Tests for discovered Hosts, following a paired Host to a new address, and Reconfigure."""

from __future__ import annotations

import asyncio
from collections.abc import AsyncIterator
from ipaddress import ip_address
from pathlib import Path

import pytest
from homeassistant import config_entries
from homeassistant.const import CONF_HOST, CONF_PORT
from homeassistant.core import HomeAssistant
from homeassistant.data_entry_flow import FlowResultType
from homeassistant.helpers.service_info.zeroconf import ZeroconfServiceInfo

from custom_components.hostbeacon.const import CONF_CODE, DOMAIN

from .fake_agent import FakeAgent
from .test_host import add_host, state, wait_for


def announcement(agent: FakeAgent, instance_id: str | None = None) -> ZeroconfServiceInfo:
    """What Home Assistant's zeroconf gives the flow for the fake Agent."""
    return ZeroconfServiceInfo(
        ip_address=ip_address("127.0.0.1"),
        ip_addresses=[ip_address("127.0.0.1")],
        port=agent.port,
        hostname="hostbeacon-12345678.local.",
        type="_hostbeacon._tcp.local.",
        name=f"{agent.hostname}._hostbeacon._tcp.local.",
        properties={"id": instance_id or agent.instance_id},
    )


async def discover(hass: HomeAssistant, info: ZeroconfServiceInfo):
    return await hass.config_entries.flow.async_init(
        DOMAIN, context={"source": config_entries.SOURCE_ZEROCONF}, data=info
    )


@pytest.fixture
async def other_agent(tmp_path: Path, socket_enabled: None) -> AsyncIterator[FakeAgent]:
    """A second fake Agent on another port."""
    directory = tmp_path / "other"
    directory.mkdir()
    fake = FakeAgent(directory)
    yield fake
    await fake.stop()


async def move(agent: FakeAgent) -> None:
    """Restart the Agent on a new port, as after an address change."""
    await agent.stop()
    agent.port = 0
    await agent.start()


async def test_discovered_host_is_paired_with_a_code(hass: HomeAssistant, agent: FakeAgent) -> None:
    code = agent.new_code()
    result = await discover(hass, announcement(agent))
    assert result["type"] is FlowResultType.FORM
    assert result["step_id"] == "pair"
    flow = next(f for f in hass.config_entries.flow.async_progress() if f["flow_id"] == result["flow_id"])
    assert flow["context"]["title_placeholders"] == {"name": "test-host"}
    assert not any(line.startswith("POST") for line, _ in agent.requests)  # nothing happens before the code

    result = await hass.config_entries.flow.async_configure(result["flow_id"], {CONF_CODE: code})
    assert result["type"] is FlowResultType.CREATE_ENTRY
    entry = result["result"]
    assert entry.unique_id == agent.instance_id
    assert entry.data[CONF_HOST] == "127.0.0.1"
    assert entry.data[CONF_PORT] == agent.port
    await wait_for(lambda: state(hass, entry, "host_status") == "online")


async def test_announcement_without_an_instance_id_is_ignored(hass: HomeAssistant, agent: FakeAgent) -> None:
    result = await discover(hass, announcement(agent, instance_id="not-an-id"))
    assert result["type"] is FlowResultType.ABORT
    assert result["reason"] == "invalid_discovery_info"


async def test_paired_host_moves_to_a_new_address_when_pin_and_key_match(
    hass: HomeAssistant, agent: FakeAgent
) -> None:
    entry = await add_host(hass, agent)
    await move(agent)
    await wait_for(lambda: state(hass, entry, "host_status") == "offline")

    result = await discover(hass, announcement(agent))
    assert result["type"] is FlowResultType.ABORT
    assert result["reason"] == "already_configured"
    assert entry.data[CONF_PORT] == agent.port
    await wait_for(lambda: state(hass, entry, "host_status") == "online")


async def test_paired_host_does_not_move_to_a_different_certificate(
    hass: HomeAssistant, agent: FakeAgent, other_agent: FakeAgent
) -> None:
    entry = await add_host(hass, agent)
    old_port = agent.port
    await agent.stop()
    await wait_for(lambda: state(hass, entry, "host_status") == "offline")
    other_agent.instance_id = agent.instance_id  # announces the same ID, but has its own certificate
    await other_agent.start()

    result = await discover(hass, announcement(other_agent))
    assert result["reason"] == "already_configured"
    assert entry.data[CONF_PORT] == old_port
    assert other_agent.requests == []  # the key was never sent


async def test_paired_host_does_not_move_when_the_key_is_refused(
    hass: HomeAssistant, agent: FakeAgent, other_agent: FakeAgent
) -> None:
    entry = await add_host(hass, agent)
    old_port = agent.port
    await agent.stop()
    await wait_for(lambda: state(hass, entry, "host_status") == "offline")
    other_agent.share_identity(agent)  # same certificate, but it does not know the key
    await other_agent.start()

    result = await discover(hass, announcement(other_agent))
    assert result["reason"] == "already_configured"
    assert entry.data[CONF_PORT] == old_port


async def test_online_host_stays_on_its_address(
    hass: HomeAssistant, agent: FakeAgent, other_agent: FakeAgent
) -> None:
    """A Host with two addresses, or a copy of it, does not pull the entry away."""
    entry = await add_host(hass, agent)
    other_agent.share_identity(agent)
    other_agent.keys = set(agent.keys)
    await other_agent.start()

    result = await discover(hass, announcement(other_agent))
    assert result["reason"] == "already_configured"
    assert entry.data[CONF_PORT] == agent.port
    assert other_agent.requests == []


async def reconfigure(hass: HomeAssistant, entry: config_entries.ConfigEntry, port: int):
    result = await hass.config_entries.flow.async_init(
        DOMAIN, context={"source": config_entries.SOURCE_RECONFIGURE, "entry_id": entry.entry_id}
    )
    assert result["type"] is FlowResultType.FORM
    assert result["step_id"] == "reconfigure"
    return await hass.config_entries.flow.async_configure(
        result["flow_id"], {CONF_HOST: "127.0.0.1", CONF_PORT: port}
    )


async def test_reconfigure_to_the_same_certificate_changes_the_address(
    hass: HomeAssistant, agent: FakeAgent
) -> None:
    entry = await add_host(hass, agent)
    await move(agent)

    result = await reconfigure(hass, entry, agent.port)
    assert result["type"] is FlowResultType.ABORT
    assert result["reason"] == "reconfigure_successful"
    assert entry.data[CONF_PORT] == agent.port
    await wait_for(lambda: state(hass, entry, "host_status") == "online")


async def test_reconfigure_to_a_different_certificate_is_refused(
    hass: HomeAssistant, agent: FakeAgent, other_agent: FakeAgent
) -> None:
    entry = await add_host(hass, agent)
    old_data = dict(entry.data)
    await other_agent.start()

    result = await reconfigure(hass, entry, other_agent.port)
    assert result["type"] is FlowResultType.FORM
    assert result["errors"] == {"base": "certificate_changed"}
    assert dict(entry.data) == old_data
    assert other_agent.requests == []


async def test_reconfigure_to_an_address_without_an_agent(
    hass: HomeAssistant, agent: FakeAgent, other_agent: FakeAgent
) -> None:
    entry = await add_host(hass, agent)
    await other_agent.start()
    port = other_agent.port
    await other_agent.stop()

    result = await reconfigure(hass, entry, port)
    assert result["errors"] == {"base": "cannot_connect"}
    await asyncio.sleep(0)
    assert entry.data[CONF_PORT] == agent.port


async def test_add_by_address_works_while_the_host_waits_in_discovered(
    hass: HomeAssistant, agent: FakeAgent
) -> None:
    discovered = await discover(hass, announcement(agent))
    assert discovered["step_id"] == "pair"

    entry = await add_host(hass, agent)
    assert entry.unique_id == agent.instance_id
    await hass.async_block_till_done()
    assert not any(f["flow_id"] == discovered["flow_id"] for f in hass.config_entries.flow.async_progress())

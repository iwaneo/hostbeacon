"""Tests for adding a Host by address and showing it in Home Assistant."""

from __future__ import annotations

import asyncio
import base64
import logging
from collections.abc import Callable

from homeassistant import config_entries
from homeassistant.const import CONF_HOST, CONF_PORT, STATE_UNAVAILABLE
from homeassistant.core import HomeAssistant
from homeassistant.data_entry_flow import FlowResultType
from homeassistant.helpers import device_registry as dr
from homeassistant.helpers import entity_registry as er

from custom_components.hostbeacon.const import CONF_CODE, CONF_FINGERPRINT, CONF_KEY, DOMAIN

from .fake_agent import FakeAgent, system


async def wait_for(check: Callable[[], bool], timeout: float = 5) -> None:
    async with asyncio.timeout(timeout):
        while not check():
            await asyncio.sleep(0.02)


async def add_host(hass: HomeAssistant, agent: FakeAgent) -> config_entries.ConfigEntry:
    """Add the fake Agent's Host by address and wait until it is Online."""
    code = agent.new_code()
    result = await hass.config_entries.flow.async_init(DOMAIN, context={"source": config_entries.SOURCE_USER})
    assert result["type"] is FlowResultType.FORM
    assert result["step_id"] == "user"
    result = await hass.config_entries.flow.async_configure(
        result["flow_id"], {CONF_HOST: "127.0.0.1", CONF_PORT: agent.port}
    )
    assert result["type"] is FlowResultType.FORM
    assert result["step_id"] == "pair"
    result = await hass.config_entries.flow.async_configure(result["flow_id"], {CONF_CODE: code})
    assert result["type"] is FlowResultType.CREATE_ENTRY, result
    entry = result["result"]
    await wait_for(lambda: state(hass, entry, "host_status") == "online")
    return entry


def entity_id(hass: HomeAssistant, entry: config_entries.ConfigEntry, key: str) -> str:
    found = er.async_get(hass).async_get_entity_id("sensor", DOMAIN, f"{entry.unique_id}_{key}")
    assert found, key
    return found


def state(hass: HomeAssistant, entry: config_entries.ConfigEntry, key: str) -> str | None:
    found = er.async_get(hass).async_get_entity_id("sensor", DOMAIN, f"{entry.unique_id}_{key}")
    current = hass.states.get(found) if found else None
    return current.state if current else None


async def test_add_host_by_address(hass: HomeAssistant, agent: FakeAgent) -> None:
    """The config flow creates one device with Host status, CPU usage, and Memory usage."""
    entry = await add_host(hass, agent)

    assert entry.title == "test-host"
    assert entry.unique_id == agent.instance_id
    assert entry.data[CONF_HOST] == "127.0.0.1"
    assert entry.data[CONF_PORT] == agent.port
    assert CONF_CODE not in entry.data

    devices = dr.async_entries_for_config_entry(dr.async_get(hass), entry.entry_id)
    assert len(devices) == 1
    assert devices[0].identifiers == {(DOMAIN, agent.instance_id)}
    assert devices[0].name == "test-host"

    assert state(hass, entry, "host_status") == "online"
    assert state(hass, entry, "cpu_usage") == "12.0"
    assert state(hass, entry, "memory_usage") == "34.0"
    assert hass.states.get(entity_id(hass, entry, "cpu_usage")).attributes["unit_of_measurement"] == "%"
    swap = er.async_get(hass).async_get(entity_id(hass, entry, "swap_usage"))
    assert swap.disabled_by is er.RegistryEntryDisabler.INTEGRATION


async def test_delta_updates_the_sensors(hass: HomeAssistant, agent: FakeAgent) -> None:
    entry = await add_host(hass, agent)
    await agent.send_system(system(cpu=55.0, memory=60.0))
    await wait_for(lambda: state(hass, entry, "cpu_usage") == "55.0")
    assert state(hass, entry, "memory_usage") == "60.0"


async def test_offline_when_the_agent_disconnects_and_back_on_reconnect(hass: HomeAssistant, agent: FakeAgent) -> None:
    entry = await add_host(hass, agent)

    await agent.stop()
    await wait_for(lambda: state(hass, entry, "host_status") == "offline")
    assert state(hass, entry, "cpu_usage") == STATE_UNAVAILABLE
    assert state(hass, entry, "memory_usage") == STATE_UNAVAILABLE

    agent.system = system(cpu=21.0)
    await agent.start()
    await wait_for(lambda: state(hass, entry, "host_status") == "online")
    assert state(hass, entry, "cpu_usage") == "21.0"


async def test_different_certificate_is_refused(hass: HomeAssistant, agent: FakeAgent, caplog) -> None:
    """A different certificate at the address gets no key and stays Offline."""
    entry = await add_host(hass, agent)
    await agent.stop()
    agent.new_certificate()
    agent.requests.clear()
    await agent.start()

    await wait_for(lambda: "certificate" in caplog.text)
    await asyncio.sleep(0.5)  # several reconnect tries
    assert state(hass, entry, "host_status") == "offline"
    assert agent.requests == []  # TLS ended before any request, so the key was never sent
    assert agent.connected == 0


async def test_wrong_code_is_refused(hass: HomeAssistant, agent: FakeAgent) -> None:
    agent.new_code()
    result = await hass.config_entries.flow.async_init(DOMAIN, context={"source": config_entries.SOURCE_USER})
    result = await hass.config_entries.flow.async_configure(
        result["flow_id"], {CONF_HOST: "127.0.0.1", CONF_PORT: agent.port}
    )
    result = await hass.config_entries.flow.async_configure(result["flow_id"], {CONF_CODE: "AAAA-AAAA-AAAA"})
    assert result["type"] is FlowResultType.FORM
    assert result["errors"] == {"base": "invalid_code"}


async def test_code_that_cannot_be_a_code_is_not_sent(hass: HomeAssistant, agent: FakeAgent) -> None:
    result = await hass.config_entries.flow.async_init(DOMAIN, context={"source": config_entries.SOURCE_USER})
    result = await hass.config_entries.flow.async_configure(
        result["flow_id"], {CONF_HOST: "127.0.0.1", CONF_PORT: agent.port}
    )
    result = await hass.config_entries.flow.async_configure(result["flow_id"], {CONF_CODE: "OOPS"})
    assert result["errors"] == {"base": "invalid_code"}
    assert not any(line.startswith("POST") for line, _ in agent.requests)


async def test_address_without_an_agent(hass: HomeAssistant, agent: FakeAgent) -> None:
    port = agent.port
    await agent.stop()
    result = await hass.config_entries.flow.async_init(DOMAIN, context={"source": config_entries.SOURCE_USER})
    result = await hass.config_entries.flow.async_configure(result["flow_id"], {CONF_HOST: "127.0.0.1", CONF_PORT: port})
    assert result["type"] is FlowResultType.FORM
    assert result["errors"] == {"base": "cannot_connect"}


async def test_same_host_twice_is_refused(hass: HomeAssistant, agent: FakeAgent) -> None:
    await add_host(hass, agent)
    code = agent.new_code()
    result = await hass.config_entries.flow.async_init(DOMAIN, context={"source": config_entries.SOURCE_USER})
    result = await hass.config_entries.flow.async_configure(
        result["flow_id"], {CONF_HOST: "127.0.0.1", CONF_PORT: agent.port}
    )
    result = await hass.config_entries.flow.async_configure(result["flow_id"], {CONF_CODE: code})
    assert result["type"] is FlowResultType.ABORT
    assert result["reason"] == "already_configured"


async def test_no_key_or_code_in_logs_or_urls(hass: HomeAssistant, agent: FakeAgent, caplog) -> None:
    caplog.set_level(logging.DEBUG)
    code = agent.new_code()
    result = await hass.config_entries.flow.async_init(DOMAIN, context={"source": config_entries.SOURCE_USER})
    result = await hass.config_entries.flow.async_configure(
        result["flow_id"], {CONF_HOST: "127.0.0.1", CONF_PORT: agent.port}
    )
    result = await hass.config_entries.flow.async_configure(result["flow_id"], {CONF_CODE: code})
    entry = result["result"]
    await wait_for(lambda: state(hass, entry, "host_status") == "online")
    await agent.stop()
    await wait_for(lambda: state(hass, entry, "host_status") == "offline")

    key = base64.b64decode(entry.data[CONF_KEY])
    secrets = [code, code.replace("-", ""), entry.data[CONF_KEY], key.hex()]
    for secret in secrets:
        assert secret not in caplog.text
        for line, _ in agent.requests:
            assert secret not in line
    assert entry.data[CONF_FINGERPRINT] == agent.fingerprint.hex()

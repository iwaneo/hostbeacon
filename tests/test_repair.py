"""Tests for Re-pair, the certificate repair, and the two-machines repair."""

from __future__ import annotations

import base64
from collections.abc import AsyncIterator
from pathlib import Path

import pytest
from homeassistant import config_entries
from homeassistant.components.repairs import repairs_flow_manager
from homeassistant.const import CONF_HOST, CONF_PORT
from homeassistant.core import HomeAssistant
from homeassistant.data_entry_flow import FlowResultType
from homeassistant.helpers import device_registry as dr
from homeassistant.helpers import entity_registry as er
from homeassistant.helpers import issue_registry as ir
from homeassistant.setup import async_setup_component

from custom_components.hostbeacon.const import CONF_CODE, CONF_FINGERPRINT, CONF_INSTANCE_ID, CONF_KEY, DOMAIN

from .fake_agent import FakeAgent
from .test_discovery import announcement, discover
from .test_host import add_host, entity_id, state, wait_for


@pytest.fixture
async def other_agent(tmp_path: Path, socket_enabled: None) -> AsyncIterator[FakeAgent]:
    """A second fake Agent on another port."""
    directory = tmp_path / "other"
    directory.mkdir()
    fake = FakeAgent(directory)
    yield fake
    await fake.stop()


def issue(hass: HomeAssistant, kind: str, entry: config_entries.ConfigEntry) -> ir.IssueEntry | None:
    return ir.async_get(hass).async_get_issue(DOMAIN, f"{kind}_{entry.entry_id}")


async def reinstall(hass: HomeAssistant, agent: FakeAgent, entry: config_entries.ConfigEntry) -> None:
    """Reinstall the Agent: new identity, no Pairings. Home Assistant raises the certificate repair."""
    await agent.stop()
    agent.reinstall()
    await agent.start()
    await wait_for(lambda: issue(hass, "certificate_changed", entry) is not None)


async def fix_certificate_repair(hass: HomeAssistant, entry: config_entries.ConfigEntry):
    """Press Fix on the certificate repair; return the Re-pair flow it starts."""
    assert await async_setup_component(hass, "repairs", {})
    manager = repairs_flow_manager(hass)
    result = await manager.async_init(DOMAIN, data={"issue_id": f"certificate_changed_{entry.entry_id}"})
    assert result["type"] is FlowResultType.FORM
    assert result["step_id"] == "confirm"
    assert result["description_placeholders"]["host"] == entry.title
    result = await manager.async_configure(result["flow_id"], {})
    assert result["type"] is FlowResultType.ABORT
    assert result["reason"] == "repair_started"
    await hass.async_block_till_done()
    flows = [f for f in hass.config_entries.flow.async_progress_by_handler(DOMAIN) if f["context"]["source"] == "reauth"]
    assert len(flows) == 1
    assert flows[0]["step_id"] == "reauth_confirm"
    return flows[0]


async def enter_code(hass: HomeAssistant, flow_id: str, agent: FakeAgent):
    return await hass.config_entries.flow.async_configure(
        flow_id, {CONF_HOST: "127.0.0.1", CONF_PORT: agent.port, CONF_CODE: agent.new_code()}
    )


async def test_repair_of_a_reinstalled_host_keeps_its_device_and_entities(
    hass: HomeAssistant, agent: FakeAgent
) -> None:
    entry = await add_host(hass, agent)
    host_id = entry.unique_id
    device = dr.async_get(hass).async_get_device_by_identifier((DOMAIN, host_id), entry.entry_id)
    cpu = entity_id(hass, entry, "cpu_usage")
    old_key = entry.data[CONF_KEY]
    await reinstall(hass, agent, entry)
    await wait_for(lambda: state(hass, entry, "host_status") == "offline")

    flow = await fix_certificate_repair(hass, entry)
    assert flow["context"]["title_placeholders"] == {"name": "test-host"}
    result = await enter_code(hass, flow["flow_id"], agent)
    assert result["type"] is FlowResultType.FORM
    assert result["step_id"] == "reauth_same_host"
    assert result["description_placeholders"]["hostname"] == "test-host"

    result = await hass.config_entries.flow.async_configure(result["flow_id"], {})
    assert result["type"] is FlowResultType.ABORT
    assert result["reason"] == "reauth_successful"
    assert entry.unique_id == host_id
    assert entry.data[CONF_INSTANCE_ID] == agent.instance_id != host_id
    assert entry.data[CONF_FINGERPRINT] == agent.fingerprint.hex()
    assert entry.data[CONF_KEY] != old_key
    await wait_for(lambda: state(hass, entry, "host_status") == "online")
    assert dr.async_get(hass).async_get_device_by_identifier((DOMAIN, host_id), entry.entry_id).id == device.id
    assert entity_id(hass, entry, "cpu_usage") == cpu
    assert len(er.async_entries_for_config_entry(er.async_get(hass), entry.entry_id)) > 0
    await wait_for(lambda: issue(hass, "certificate_changed", entry) is None)
    # The Agent learns the Host ID, so a copy of it can report it.
    assert agent.host_ids[-1] == host_id


async def test_repair_removes_the_old_pairing_when_the_old_key_still_works(
    hass: HomeAssistant, agent: FakeAgent
) -> None:
    entry = await add_host(hass, agent)
    await agent.stop()
    agent.new_certificate()  # the Agent kept the old Pairing
    await agent.start()
    await wait_for(lambda: issue(hass, "certificate_changed", entry) is not None)
    assert len(agent.keys) == 1

    flow = await fix_certificate_repair(hass, entry)
    result = await enter_code(hass, flow["flow_id"], agent)
    result = await hass.config_entries.flow.async_configure(result["flow_id"], {})
    assert result["reason"] == "reauth_successful"
    assert agent.keys == {base64.b64decode(entry.data[CONF_KEY])}


async def test_repair_of_a_copy_needs_the_tick(hass: HomeAssistant, agent: FakeAgent) -> None:
    entry = await add_host(hass, agent)
    await reinstall(hass, agent, entry)
    agent.copied_from = [entry.unique_id]

    flow = await fix_certificate_repair(hass, entry)
    result = await enter_code(hass, flow["flow_id"], agent)
    assert result["step_id"] == "reauth_copy"
    result = await hass.config_entries.flow.async_configure(result["flow_id"], {"same_machine": False})
    assert result["type"] is FlowResultType.FORM
    assert result["errors"] == {"base": "same_machine_required"}
    assert entry.data[CONF_FINGERPRINT] != agent.fingerprint.hex()

    result = await hass.config_entries.flow.async_configure(result["flow_id"], {"same_machine": True})
    assert result["reason"] == "reauth_successful"
    await wait_for(lambda: state(hass, entry, "host_status") == "online")


async def test_repair_is_refused_for_an_agent_of_another_host(
    hass: HomeAssistant, agent: FakeAgent, other_agent: FakeAgent
) -> None:
    entry = await add_host(hass, agent)
    await other_agent.start()
    other = await add_host(hass, other_agent)
    await reinstall(hass, agent, entry)
    old_data = dict(entry.data)

    flow = await fix_certificate_repair(hass, entry)
    result = await enter_code(hass, flow["flow_id"], other_agent)
    assert result["type"] is FlowResultType.FORM
    assert result["errors"] == {"base": "other_host"}
    assert dict(entry.data) == old_data
    assert state(hass, other, "host_status") == "online"


async def test_reconfigure_to_a_different_certificate_hands_off_to_repair(
    hass: HomeAssistant, agent: FakeAgent
) -> None:
    entry = await add_host(hass, agent)
    host_id = entry.unique_id
    await agent.stop()
    agent.reinstall()
    agent.port = 0
    await agent.start()

    result = await hass.config_entries.flow.async_init(
        DOMAIN, context={"source": config_entries.SOURCE_RECONFIGURE, "entry_id": entry.entry_id}
    )
    result = await hass.config_entries.flow.async_configure(
        result["flow_id"], {CONF_HOST: "127.0.0.1", CONF_PORT: agent.port}
    )
    assert result["type"] is FlowResultType.FORM
    assert result["step_id"] == "reauth_confirm"
    result = await enter_code(hass, result["flow_id"], agent)
    result = await hass.config_entries.flow.async_configure(result["flow_id"], {})
    assert result["type"] is FlowResultType.ABORT
    assert result["reason"] == "reauth_successful"
    assert entry.unique_id == host_id
    assert entry.data[CONF_PORT] == agent.port
    await wait_for(lambda: state(hass, entry, "host_status") == "online")


async def test_re_paired_host_is_not_offered_as_a_new_host(hass: HomeAssistant, agent: FakeAgent) -> None:
    entry = await add_host(hass, agent)
    await reinstall(hass, agent, entry)
    discovered = await discover(hass, announcement(agent))  # the new instance ID looks like a new Host
    assert discovered["step_id"] == "pair"
    flow = await fix_certificate_repair(hass, entry)
    result = await enter_code(hass, flow["flow_id"], agent)
    await hass.config_entries.flow.async_configure(result["flow_id"], {})
    await hass.async_block_till_done()
    assert not any(f["flow_id"] == discovered["flow_id"] for f in hass.config_entries.flow.async_progress())

    result = await discover(hass, announcement(agent))
    assert result["type"] is FlowResultType.ABORT
    assert result["reason"] == "already_configured"


async def test_two_machines_with_one_identity_raise_a_repair(
    hass: HomeAssistant, agent: FakeAgent, other_agent: FakeAgent
) -> None:
    entry = await add_host(hass, agent)
    other_agent.share_identity(agent)  # an undetected copy: same identity and keys
    other_agent.keys = set(agent.keys)
    await other_agent.start()

    result = await discover(hass, announcement(other_agent))
    assert result["reason"] == "already_configured"
    found = issue(hass, "two_machines", entry)
    assert found is not None
    assert found.severity is ir.IssueSeverity.ERROR
    assert found.translation_placeholders == {"host": "test-host"}
    assert entry.data[CONF_PORT] == agent.port  # stays on the saved address


async def test_same_agent_at_a_second_address_raises_no_repair(
    hass: HomeAssistant, agent: FakeAgent, other_agent: FakeAgent
) -> None:
    entry = await add_host(hass, agent)
    other_agent.share_identity(agent)
    other_agent.keys = set(agent.keys)
    other_agent.run_id = agent.run_id  # the same running Agent, reached at another address
    await other_agent.start()

    await discover(hass, announcement(other_agent))
    assert issue(hass, "two_machines", entry) is None
    assert entry.data[CONF_PORT] == agent.port


async def test_refused_key_starts_re_pair(hass: HomeAssistant, agent: FakeAgent) -> None:
    """After a leaked Home Assistant key, the owner removes the Pairing on the Host, then Re-pairs."""
    entry = await add_host(hass, agent)
    agent.keys.clear()
    await agent.stop()
    await agent.start()
    await wait_for(
        lambda: any(
            f["context"]["source"] == "reauth" for f in hass.config_entries.flow.async_progress_by_handler(DOMAIN)
        )
    )
    flow = next(f for f in hass.config_entries.flow.async_progress_by_handler(DOMAIN))
    result = await enter_code(hass, flow["flow_id"], agent)
    result = await hass.config_entries.flow.async_configure(result["flow_id"], {})
    assert result["reason"] == "reauth_successful"
    await wait_for(lambda: state(hass, entry, "host_status") == "online")

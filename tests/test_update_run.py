"""Tests for the Update run from the Updates entity: Install, progress, result,
Host status Updating, the Last Update run sensor, and repairs 1 to 3 (v1 spec §7.3, §7.7, §8)."""

from __future__ import annotations

import asyncio
import json
from datetime import timedelta
from typing import Any

import pytest
from homeassistant.components.update import DOMAIN as UPDATE_DOMAIN, SERVICE_INSTALL
from homeassistant.config_entries import ConfigEntry
from homeassistant.const import ATTR_ENTITY_ID, STATE_UNKNOWN
from homeassistant.core import Context, HomeAssistant
from homeassistant.exceptions import HomeAssistantError, Unauthorized
from homeassistant.helpers import issue_registry as ir
from homeassistant.util import dt as dt_util
from pytest_homeassistant_custom_component.common import MockUser, async_fire_time_changed

from custom_components.hostbeacon import protocol
from custom_components.hostbeacon.const import DOMAIN

from .fake_agent import FakeAgent, flags, update_run
from .test_host import add_host, entity_id, state, wait_for
from .test_updates import PACKAGES, release_notes, updates, updates_entity_id, updates_state

RUN_ID = "0a5b4c3d-2e1f-4a0b-9c8d-7e6f5a4b3c2d"
OTHER_RUN_ID = "7e6f5a4b-3c2d-4a0b-9c8d-0a5b4c3d2e1f"
MANUAL_NAMES = ["hostbeacon-test-removed1", "hostbeacon-test-removed2"]


def running(percent: float | None = None, started: str = "2026-09-29T12:00:00Z", run_id: str = RUN_ID) -> protocol.UpdateRun:
    return update_run("running", run_id=run_id, started_at=started, percent=percent,
                      needs_manual_update=protocol.NameList(count=0, names=[]))


def finished(result: str, run_id: str = RUN_ID, **fields: Any) -> protocol.UpdateRun:
    values: dict[str, Any] = {
        "run_id": run_id,
        "started_at": "2026-09-29T12:00:00Z",
        "finished_at": "2026-09-29T12:05:00Z",
        "result": result,
        "needs_manual_update": protocol.NameList(count=0, names=[]),
    }
    return update_run("finished", **(values | fields))


def needs_manual(run_id: str = RUN_ID) -> protocol.UpdateRun:
    return finished(
        "needs_manual_update",
        run_id=run_id,
        error="The Update run would remove packages, so nothing was installed.",
        needs_manual_update=protocol.NameList(count=2, names=MANUAL_NAMES),
    )


async def add_update_run_host(hass: HomeAssistant, agent: FakeAgent, enabled: bool = True) -> ConfigEntry:
    agent.capabilities = ["available_updates"]
    agent.enabled_actions = ["update_run"] if enabled else []
    agent.groups = protocol.Groups(flags=flags(), available_updates=updates(2))
    return await add_host(hass, agent)


def install(hass: HomeAssistant, entry: ConfigEntry, user_id: str | None = None) -> asyncio.Task[Any]:
    """Start Install; the call waits until the run ends."""
    return hass.async_create_task(
        hass.services.async_call(
            UPDATE_DOMAIN,
            SERVICE_INSTALL,
            {ATTR_ENTITY_ID: updates_entity_id(hass, entry)},
            blocking=True,
            context=Context(user_id=user_id),
        )
    )


def issue(hass: HomeAssistant, kind: str, entry: ConfigEntry) -> ir.IssueEntry | None:
    return ir.async_get(hass).async_get_issue(DOMAIN, f"{kind}_{entry.entry_id}")


async def test_install_is_offered_only_when_update_run_is_enabled(hass: HomeAssistant, agent: FakeAgent) -> None:
    entry = await add_update_run_host(hass, agent, enabled=False)
    # Release notes only.
    assert updates_state(hass, entry).attributes["supported_features"] == 16

    agent.enabled_actions = ["update_run"]
    await agent.send_groups(protocol.Groups(agent=agent._agent_info()))
    # Install, progress, and release notes.
    await wait_for(lambda: updates_state(hass, entry).attributes["supported_features"] == 1 | 4 | 16)


async def test_no_install_without_full_support(hass: HomeAssistant, agent: FakeAgent) -> None:
    agent.enabled_actions = ["update_run"]
    entry = await add_host(hass, agent)
    assert updates_state(hass, entry).attributes["supported_features"] == 16


async def test_install_runs_an_update_run_with_progress_until_it_ends(
    hass: HomeAssistant, agent: FakeAgent, hass_admin_user: MockUser
) -> None:
    entry = await add_update_run_host(hass, agent)
    assert state(hass, entry, "last_update_run") == "none_yet"

    task = install(hass, entry, hass_admin_user.id)
    await wait_for(lambda: len(agent.action_requests) == 1)
    request = agent.action_requests[0]
    assert (request.action, request.user) == ("update_run", hass_admin_user.name)
    await wait_for(lambda: updates_state(hass, entry).attributes["in_progress"] is True)

    await agent.send_update_run(update_run("waiting_for_lock", run_id=RUN_ID, started_at="2026-09-29T12:00:00Z",
                                           needs_manual_update=protocol.NameList(count=0, names=[])))
    await wait_for(lambda: state(hass, entry, "host_status") == "updating")
    await agent.send_update_run(running(percent=40))
    await wait_for(lambda: updates_state(hass, entry).attributes["update_percentage"] == 40)
    assert state(hass, entry, "host_status") == "updating"
    assert not task.done()

    await agent.send_update_run(finished("ok", installed=2, remaining=0))
    await asyncio.wait_for(task, 5)
    await hass.async_block_till_done()
    assert state(hass, entry, "host_status") == "online"
    assert updates_state(hass, entry).attributes["in_progress"] is False
    last = hass.states.get(entity_id(hass, entry, "last_update_run"))
    assert last.state == "ok"
    assert last.attributes["installed"] == 2
    assert last.attributes["remaining"] == 0
    assert last.attributes["finished_at"] == "2026-09-29T12:05:00Z"
    assert last.attributes["friendly_name"] == "test-host Last Update run"
    # Details are not saved in HA history.
    assert {"finished_at", "installed", "remaining", "error"} <= last.state_info["unrecorded_attributes"]


async def test_last_update_run_keeps_the_last_result_while_a_new_run_goes_on(
    hass: HomeAssistant, agent: FakeAgent
) -> None:
    agent.update_run = finished("failed", error="The install failed: exit status 100")
    entry = await add_update_run_host(hass, agent)
    assert state(hass, entry, "last_update_run") == "failed"

    await agent.send_update_run(running(run_id=OTHER_RUN_ID))
    await wait_for(lambda: state(hass, entry, "host_status") == "updating")
    assert state(hass, entry, "last_update_run") == "failed"


async def test_failed_run_is_an_install_error(hass: HomeAssistant, agent: FakeAgent) -> None:
    entry = await add_update_run_host(hass, agent)
    task = install(hass, entry)
    await wait_for(lambda: len(agent.action_requests) == 1)
    await agent.send_update_run(finished("failed", error="Cannot refresh the package list: exit status 100", remaining=2))

    with pytest.raises(HomeAssistantError, match="The Update run on test-host failed: Cannot refresh the package list"):
        await asyncio.wait_for(task, 5)
    assert state(hass, entry, "last_update_run") == "failed"


async def test_needs_manual_update_is_an_install_error_and_a_repair_without_names(
    hass: HomeAssistant, agent: FakeAgent, hass_ws_client
) -> None:
    entry = await add_update_run_host(hass, agent)
    task = install(hass, entry)
    await wait_for(lambda: len(agent.action_requests) == 1)
    await agent.send_update_run(needs_manual())

    with pytest.raises(HomeAssistantError, match="test-host needs a manual update"):
        await asyncio.wait_for(task, 5)
    assert state(hass, entry, "last_update_run") == "needs_manual_update"
    found = issue(hass, "needs_manual_update", entry)
    assert found is not None
    assert found.severity is ir.IssueSeverity.WARNING
    assert_no_names(hass)

    # The names and the command are in the release notes only.
    notes = await release_notes(hass, hass_ws_client, entry)
    assert "**Needs manual update:** an Update run stopped before it installed anything" in notes
    assert "`hostbeacon-test-removed1`, `hostbeacon-test-removed2`" in notes
    assert "`sudo apt full-upgrade`" in notes


async def test_needs_manual_update_repair_stays_during_a_new_run_and_clears_after_it_passes(
    hass: HomeAssistant, agent: FakeAgent
) -> None:
    agent.update_run = needs_manual()
    entry = await add_update_run_host(hass, agent)
    assert issue(hass, "needs_manual_update", entry) is not None

    await agent.send_update_run(running(run_id=OTHER_RUN_ID))
    await wait_for(lambda: state(hass, entry, "host_status") == "updating")
    assert issue(hass, "needs_manual_update", entry) is not None

    # Failed before its checks: the Agent keeps the names, so the repair stays.
    kept = protocol.NameList(count=2, names=MANUAL_NAMES)
    await agent.send_update_run(finished("failed", run_id=OTHER_RUN_ID, error="Cannot refresh", needs_manual_update=kept))
    await wait_for(lambda: state(hass, entry, "last_update_run") == "failed")
    assert issue(hass, "needs_manual_update", entry) is not None

    await agent.send_update_run(finished("ok", run_id=RUN_ID, installed=2, remaining=0))
    await wait_for(lambda: issue(hass, "needs_manual_update", entry) is None)


async def test_needs_manual_update_repair_clears_when_no_updates_are_left(hass: HomeAssistant, agent: FakeAgent) -> None:
    agent.update_run = needs_manual()
    entry = await add_update_run_host(hass, agent)
    assert issue(hass, "needs_manual_update", entry) is not None

    await agent.send_groups(protocol.Groups(available_updates=updates(0)))
    await wait_for(lambda: issue(hass, "needs_manual_update", entry) is None)


@pytest.mark.parametrize(
    ("reason", "message"),
    [
        ("update_run_running", "An Update run is already running on test-host."),
        ("busy", "test-host is busy with a package task. Try again later."),
        ("disabled", "Update run is turned off on test-host. The Host owner can turn it on in the Agent config."),
        ("cannot_log", "test-host refused: the Agent cannot write its Action log."),
    ],
)
async def test_refusals(hass: HomeAssistant, agent: FakeAgent, reason: str, message: str) -> None:
    entry = await add_update_run_host(hass, agent)
    agent.refusal = (reason, None)

    with pytest.raises(HomeAssistantError) as raised:
        await asyncio.wait_for(install(hass, entry), 5)
    # Home Assistant leaves out the final period.
    assert str(raised.value) == message.removesuffix(".")
    assert updates_state(hass, entry).attributes["in_progress"] is False


async def test_non_admin_is_refused(hass: HomeAssistant, agent: FakeAgent, hass_owner_user: MockUser) -> None:
    entry = await add_update_run_host(hass, agent)
    user = await hass.auth.async_create_user("Guest", group_ids=["system-users"])
    assert not user.is_admin

    # Home Assistant 2026.9 and later refuse before the Integration.
    with pytest.raises(Unauthorized):
        await asyncio.wait_for(install(hass, entry, user.id), 5)
    # Older versions do not, so the Integration checks itself.
    entity = hass.data["entity_components"][UPDATE_DOMAIN].get_entity(updates_entity_id(hass, entry))
    entity.async_set_context(Context(user_id=user.id))
    with pytest.raises(HomeAssistantError, match="Only Home Assistant admins can update test-host"):
        await entity.async_install(None, False)
    assert agent.action_requests == []


async def test_result_unknown_when_the_connection_ends_first_then_read_again(
    hass: HomeAssistant, agent: FakeAgent
) -> None:
    entry = await add_update_run_host(hass, agent)
    task = install(hass, entry)
    await wait_for(lambda: len(agent.action_requests) == 1)
    await agent.send_update_run(running(percent=10))
    await wait_for(lambda: state(hass, entry, "host_status") == "updating")

    # Home Assistant restarts during the run: its call ends first.
    await agent.stop()
    with pytest.raises(HomeAssistantError, match="The result of the Update run on test-host is unknown"):
        await asyncio.wait_for(task, 5)
    assert await hass.config_entries.async_unload(entry.entry_id)
    await agent.start()
    assert await hass.config_entries.async_setup(entry.entry_id)
    await wait_for(lambda: state(hass, entry, "host_status") == "updating")
    # The snapshot carries the run record, so the run shows again.
    assert updates_state(hass, entry).attributes["in_progress"] is True
    assert state(hass, entry, "last_update_run") == STATE_UNKNOWN

    await agent.send_update_run(finished("ok", installed=2, remaining=0))
    await wait_for(lambda: state(hass, entry, "last_update_run") == "ok")
    assert state(hass, entry, "host_status") == "online"


async def test_run_the_agent_could_not_finish_is_result_unknown(hass: HomeAssistant, agent: FakeAgent, hass_ws_client) -> None:
    agent.update_run = update_run(
        "result_unknown", run_id=RUN_ID, started_at="2026-09-29T12:00:00Z",
        error="The Update run stopped before it reported a result.",
        needs_manual_update=protocol.NameList(count=0, names=[]),
    )
    entry = await add_update_run_host(hass, agent)
    assert state(hass, entry, "last_update_run") == STATE_UNKNOWN
    assert state(hass, entry, "host_status") == "online"
    assert "**Last Update run:** Result unknown" in await release_notes(hass, hass_ws_client, entry)


async def test_repair_for_a_run_over_one_hour(hass: HomeAssistant, agent: FakeAgent) -> None:
    entry = await add_update_run_host(hass, agent)
    started = dt_util.utcnow() - timedelta(minutes=59)
    await agent.send_update_run(running(started=started.strftime("%Y-%m-%dT%H:%M:%SZ")))
    await wait_for(lambda: state(hass, entry, "host_status") == "updating")
    assert issue(hass, "update_run_long", entry) is None

    async_fire_time_changed(hass, dt_util.utcnow() + timedelta(minutes=2))
    await hass.async_block_till_done()
    found = issue(hass, "update_run_long", entry)
    assert found is not None
    assert found.severity is ir.IssueSeverity.WARNING

    # Never stopped: only the end of the run removes it.
    await agent.send_update_run(finished("ok", installed=2, remaining=0))
    await wait_for(lambda: issue(hass, "update_run_long", entry) is None)


async def test_repair_for_a_run_that_is_already_over_one_hour(hass: HomeAssistant, agent: FakeAgent) -> None:
    entry = await add_update_run_host(hass, agent)
    await agent.send_update_run(running(started=(dt_util.utcnow() - timedelta(hours=2)).strftime("%Y-%m-%dT%H:%M:%SZ")))
    await wait_for(lambda: issue(hass, "update_run_long", entry) is not None)


async def test_repair_for_a_broken_package_system(hass: HomeAssistant, agent: FakeAgent) -> None:
    entry = await add_update_run_host(hass, agent)
    assert issue(hass, "package_system_broken", entry) is None

    broken = protocol.Flags(
        reboot_required="no", package_task_running=False, package_system_broken=True,
        package_system_fix_command="sudo dpkg --configure -a", last_boot="2026-09-21T14:13:20Z",
    )
    await agent.send_groups(protocol.Groups(flags=broken))
    await wait_for(lambda: issue(hass, "package_system_broken", entry) is not None)
    found = issue(hass, "package_system_broken", entry)
    assert found.severity is ir.IssueSeverity.ERROR
    assert found.is_persistent
    assert found.translation_placeholders["command"] == "sudo dpkg --configure -a"

    await agent.send_groups(protocol.Groups(flags=flags()))
    await wait_for(lambda: issue(hass, "package_system_broken", entry) is None)


async def test_repairs_hold_no_package_names(hass: HomeAssistant, agent: FakeAgent) -> None:
    agent.update_run = needs_manual()
    agent.groups = protocol.Groups(flags=flags(), available_updates=updates(2))
    agent.capabilities = ["available_updates"]
    agent.enabled_actions = ["update_run"]
    await add_host(hass, agent)
    assert_no_names(hass)


def assert_no_names(hass: HomeAssistant) -> None:
    """HA saves repairs to disk, so no repair may hold a package name (v1 spec §7.4)."""
    issues = [item.to_json() for item in ir.async_get(hass).issues.values() if item.domain == DOMAIN]
    assert issues
    text = json.dumps(issues)
    for name in [*MANUAL_NAMES, *(item.name for item in PACKAGES)]:
        assert name not in text


async def test_removing_the_host_removes_its_repairs(hass: HomeAssistant, agent: FakeAgent) -> None:
    agent.update_run = needs_manual()
    entry = await add_update_run_host(hass, agent)
    assert issue(hass, "needs_manual_update", entry) is not None
    assert await hass.config_entries.async_remove(entry.entry_id)
    assert issue(hass, "needs_manual_update", entry) is None

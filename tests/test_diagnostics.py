"""Tests for "Download diagnostics": what is redacted (v1 spec §7.9)."""

from __future__ import annotations

import base64
import json

import pytest

from homeassistant.core import HomeAssistant
from homeassistant.helpers import device_registry as dr
from pytest_homeassistant_custom_component.components.diagnostics import (
    get_diagnostics_for_config_entry,
    get_diagnostics_for_device,
)
from pytest_homeassistant_custom_component.typing import ClientSessionGenerator

from custom_components.hostbeacon import protocol
from custom_components.hostbeacon.const import CONF_KEY
from custom_components.hostbeacon.diagnostics import _redact_text

from .fake_agent import FakeAgent, flags, update_run
from .test_host import add_host

HOSTNAME = "nas-attic"
SERIAL = "S5STNF0T123456A"
BY_ID = f"ata-Samsung_SSD_870_EVO_1TB_{SERIAL}"
MAC = "02:42:ac:11:00:02"
IPV4 = "192.168.7.42"
IPV6 = "2001:db8::42:1"
HOST_USER = "alice"
HA_USER = "Bob Builder"
HA_USERNAME = "bobb"


def _secret_groups() -> protocol.Groups:
    """Groups that hold every redacted kind of value, alone and inside longer text."""
    return protocol.Groups(
        flags=protocol.Flags(
            reboot_required="yes",
            package_task_running=False,
            package_system_broken=True,
            package_system_fix_command=f"sudo dpkg --configure -a  # {HOSTNAME} at {IPV4}",
            last_boot="2026-09-21T14:13:20Z",
        ),
        disks=protocol.Disks(
            mounts=[
                protocol.Mount(mount="/", used_percent=40, free_bytes=1, total_bytes=2),
                protocol.Mount(mount=f"/home/{HOST_USER}/backup", used_percent=10, free_bytes=1, total_bytes=2),
                protocol.Mount(mount=f"/mnt/{BY_ID}", used_percent=10, free_bytes=1, total_bytes=2),
            ]
        ),
        failed_services=protocol.NameList(count=2, names=[r"mnt-nas\x2dattic.mount", "nginx.service"]),
        network=protocol.Network(
            interfaces=[
                protocol.Interface(
                    name="enx0242ac110002",
                    rx_bytes_per_second=1,
                    tx_bytes_per_second=1,
                    rx_bytes_total=1,
                    tx_bytes_total=1,
                )
            ]
        ),
        containers=protocol.Containers(
            count=1,
            running=1,
            stopped=0,
            unhealthy=0,
            items=[protocol.Container(name=f"proxy-{IPV4}", state="running")],
        ),
    )


async def test_diagnostics_redact_private_values(
    hass: HomeAssistant, hass_client: ClientSessionGenerator, agent: FakeAgent
) -> None:
    """No Pairing key, Host address, hostname, IP or MAC address, disk serial, or
    user name is in the diagnostics, also not inside longer text."""
    user = await hass.auth.async_create_user(HA_USER)
    provider = hass.auth.auth_providers[0]
    credentials = await provider.async_get_or_create_credentials({"username": HA_USERNAME})
    await hass.auth.async_link_user(user, credentials)
    agent.hostname = HOSTNAME
    agent.groups = _secret_groups()
    agent.update_run = update_run(
        "finished",
        result="failed",
        error=(
            f"E: {HA_USER} ({HA_USERNAME}) on {HOSTNAME}.lan: cannot reach [{IPV6}]:80 or {IPV4}, "
            f"link {MAC}, disk /dev/disk/by-id/{BY_ID} serial number: {SERIAL}, "
            f"wwn-0x5002538e40a1b2c3, /home/{HOST_USER}/.cache, ssh {HOST_USER}@{HOSTNAME}"
        ),
    )
    entry = await add_host(hass, agent)
    key = entry.data[CONF_KEY]
    device = dr.async_entries_for_config_entry(dr.async_get(hass), entry.entry_id)[0]

    for diagnostics in (
        await get_diagnostics_for_config_entry(hass, hass_client, entry),
        await get_diagnostics_for_device(hass, hass_client, entry, device),
    ):
        text = json.dumps(diagnostics)
        for secret in (
            key,
            base64.b64decode(key).hex(),
            "127.0.0.1",
            HOSTNAME,
            "attic",
            SERIAL,
            "5002538e40a1b2c3",
            MAC,
            MAC.replace(":", ""),
            IPV4,
            IPV6,
            HOST_USER,
            HA_USER,
            HA_USERNAME,
        ):
            assert secret.lower() not in text.lower(), secret

        # What helps to find a problem stays.
        connection = diagnostics["connection"]
        assert connection["host_status"] == "online"
        assert connection["hello"]["instance_id"] == agent.instance_id
        assert connection["hello"]["kernel"] == agent.kernel
        assert connection["groups"]["flags"]["package_system_fix_command"].startswith("sudo dpkg --configure -a")
        assert "nginx.service" in connection["groups"]["failed_services"]["names"]
        assert "E: **REDACTED** (**REDACTED**) on **REDACTED**.lan: cannot reach" in text
        assert diagnostics["entry"]["data"]["port"] == agent.port


async def test_diagnostics_while_offline(
    hass: HomeAssistant, hass_client: ClientSessionGenerator, agent: FakeAgent
) -> None:
    """Diagnostics work before the Agent sent anything, and keep the problem."""
    agent.groups = protocol.Groups(flags=flags())
    entry = await add_host(hass, agent)
    await agent.stop()
    await hass.config_entries.async_reload(entry.entry_id)
    await hass.async_block_till_done()

    diagnostics = await get_diagnostics_for_config_entry(hass, hass_client, entry)

    assert diagnostics["connection"]["host_status"] == "offline"
    assert diagnostics["connection"]["hello"] is None
    assert "127.0.0.1" not in json.dumps(diagnostics)


@pytest.mark.parametrize(
    ("text", "expected"),
    [
        ("addr:fe80::1 up", "addr:**REDACTED** up"),
        ("via fe80::1%eth0.", "via **REDACTED**%eth0."),
        ("gw ::ffff:10.0.0.1:", "gw **REDACTED**:"),
        ("mac BC-24-11-B6-FA-18 ok", "mac **REDACTED** ok"),
        ("serial=ABC123, Serial Number: XYZ", "serial=**REDACTED**, Serial Number: **REDACTED**"),
        ('{"serial": "S5ST"}', '{"serial": "**REDACTED**"}'),
        ("/dev/disk/by-id/virtio-HBV1234", "/dev/disk/by-id/**REDACTED**"),
        ("added:2001:db8::1", "added:**REDACTED**"),
        ("mac 0242.ac11.0002, if enx0242ac110002", "mac **REDACTED**, if enx**REDACTED**"),
        ("tomer@pve:~$", "**REDACTED**@**REDACTED**:~$"),
        ("on pve.lan: pve-manager", "on **REDACTED**.lan: **REDACTED**-manager"),
        # Times, package versions, and package names stay.
        ("at 10:00:00, 2026-09-21T14:13:20Z", "at 10:00:00, 2026-09-21T14:13:20Z"),
        ("1:9.18.28-1~deb12u2 nvme-cli usb-modeswitch", "1:9.18.28-1~deb12u2 nvme-cli usb-modeswitch"),
        ("run 0f8e2c1a-5b7d-4e3f-9a6b-1c2d3e4f5a6b", "run 0f8e2c1a-5b7d-4e3f-9a6b-1c2d3e4f5a6b"),
    ],
)
def test_redact_inside_text(text: str, expected: str) -> None:
    assert _redact_text(text, ["pve"]) == expected

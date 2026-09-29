"""Tests for the Host sensors: capabilities, default-off entities, device info, and unrecorded names."""

from __future__ import annotations

from datetime import timedelta

from homeassistant.const import STATE_UNAVAILABLE, STATE_UNKNOWN
from homeassistant.config_entries import ConfigEntry
from homeassistant.core import HomeAssistant, State
from homeassistant.helpers import device_registry as dr
from homeassistant.helpers import entity_registry as er
from homeassistant.util import dt as dt_util

from custom_components.hostbeacon import protocol
from custom_components.hostbeacon.const import DOMAIN

from .fake_agent import FakeAgent, flags, system
from .test_host import add_host, entity_id, state, wait_for

ALL_CAPABILITIES = [
    "load",
    "temperatures",
    "disks",
    "network",
    "failed_services",
    "available_updates",
    "containers",
    "smart",
]

ROOT = protocol.Mount(mount="/", used_percent=74.0, free_bytes=25_000_000_000, total_bytes=100_000_000_000)
ETH0 = protocol.Interface(
    name="eth0",
    rx_bytes_per_second=125_000.0,
    tx_bytes_per_second=1_250.0,
    rx_bytes_total=4_700_000_000,
    tx_bytes_total=5_000,
)
PACKAGE = protocol.Package(name="bash", installed_version="5.2.37-2", new_version="5.2.37-2+b5")
CONTAINERS = protocol.Containers(
    count=4,
    running=2,
    stopped=1,
    unhealthy=1,
    items=[
        protocol.Container(name="jellyfin", state="running"),
        protocol.Container(name="old-db", state="stopped"),
        protocol.Container(name="paperless", state="unhealthy"),
        protocol.Container(name="web", state="running"),
    ],
)
NVME = protocol.SmartDisk(device="nvme0n1", health="ok", temperature_celsius=39.0, wear_percent=4.0)
# An HDD has no wear.
HDD = protocol.SmartDisk(device="sda", health="failing", temperature_celsius=44.0, wear_percent=None)

CAPABILITY_KEYS = {
    "load": ["load_1", "load_5", "load_15"],
    "temperatures": ["cpu_temperature"],
    "disks": ["disk_used_/", "disk_free_/"],
    "network": ["download_eth0", "upload_eth0", "downloaded_eth0", "uploaded_eth0"],
    "failed_services": ["failed_services"],
    "available_updates": ["available_updates", "package_list_refreshed"],
    "containers": ["containers_running", "containers_stopped", "containers_unhealthy"],
    "smart": [
        "disk_health_nvme0n1",
        "disk_temperature_nvme0n1",
        "disk_wear_nvme0n1",
        "disk_health_sda",
        "disk_temperature_sda",
    ],
}
ALWAYS_KEYS = [
    "host_status",
    "cpu_usage",
    "memory_usage",
    "swap_usage",
    "last_boot",
    "reboot_required",
    "environment",
    "kernel",
    "protocol_version",
    "last_seen",
]
DEFAULT_OFF = {
    "swap_usage",
    "load_5",
    "load_15",
    "disk_free_/",
    "downloaded_eth0",
    "uploaded_eth0",
    "environment",
    "kernel",
    "protocol_version",
    "last_seen",
    "package_list_refreshed",
}


def full_host(agent: FakeAgent, capabilities: list[str] = ALL_CAPABILITIES) -> None:
    """Give the fake Agent every group, and the capabilities it reports."""
    agent.capabilities = capabilities
    agent.system = system(load=(0.5, 0.4, 0.3))
    agent.groups = protocol.Groups(
        flags=flags("yes"),
        disks=protocol.Disks(mounts=[ROOT]),
        network=protocol.Network(interfaces=[ETH0]),
        temperatures=protocol.Temperatures(cpu_celsius=52.0),
        failed_services=protocol.NameList(count=2, names=["a.service", "b.service"]),
        available_updates=protocol.AvailableUpdates(count=1, packages=[PACKAGE], fingerprint="ab" * 32, last_refresh=None),
        containers=CONTAINERS,
        smart=protocol.Smart(disks=[NVME, HDD]),
    )


def registered(hass: HomeAssistant, host_id: str, key: str) -> er.RegistryEntry | None:
    registry = er.async_get(hass)
    found = registry.async_get_entity_id("sensor", DOMAIN, f"{host_id}_{key}") or registry.async_get_entity_id(
        "binary_sensor", DOMAIN, f"{host_id}_{key}"
    )
    return registry.async_get(found) if found else None


def binary_state(hass: HomeAssistant, entry: ConfigEntry, key: str) -> State:
    found = er.async_get(hass).async_get_entity_id("binary_sensor", DOMAIN, f"{entry.unique_id}_{key}")
    assert found, key
    return hass.states.get(found)


def enable_before_adding(hass: HomeAssistant, agent: FakeAgent, *keys: str) -> None:
    """Register default-off sensors as enabled, as a user would turn them on."""
    for key in keys:
        er.async_get(hass).async_get_or_create("sensor", DOMAIN, f"{agent.instance_id}_{key}")


async def test_every_sensor_with_its_capability(hass: HomeAssistant, agent: FakeAgent) -> None:
    full_host(agent)
    entry = await add_host(hass, agent)

    assert float(state(hass, entry, "load_1")) == 0.5
    assert float(state(hass, entry, "cpu_temperature")) == 52
    assert float(state(hass, entry, "disk_used_/")) == 74
    assert state(hass, entry, "failed_services") == "2"
    assert state(hass, entry, "available_updates") == "1"
    assert state(hass, entry, "reboot_required") == "yes"
    assert state(hass, entry, "last_boot") == "2026-09-21T14:13:20+00:00"

    download = hass.states.get(entity_id(hass, entry, "download_eth0"))
    assert float(download.state) == 1.0  # 125 000 B/s
    assert download.attributes["unit_of_measurement"] == "Mbit/s"
    assert download.attributes["friendly_name"] == "test-host eth0 download"
    disk = hass.states.get(entity_id(hass, entry, "disk_used_/"))
    assert disk.attributes["friendly_name"] == "test-host Disk / used"
    assert disk.attributes["unit_of_measurement"] == "%"
    services = hass.states.get(entity_id(hass, entry, "failed_services"))
    assert services.attributes["services"] == ["a.service", "b.service"]


async def test_each_sensor_appears_only_with_its_capability(hass: HomeAssistant, agent: FakeAgent) -> None:
    """In LXC the Agent leaves out load and temperatures; other Hosts miss other sources."""
    full_host(agent, capabilities=["disks", "network", "failed_services", "available_updates"])
    entry = await add_host(hass, agent)

    for capability, keys in CAPABILITY_KEYS.items():
        for key in keys:
            present = registered(hass, entry.unique_id, key) is not None
            assert present == (capability in agent.capabilities), key
    for key in ALWAYS_KEYS:
        assert registered(hass, entry.unique_id, key) is not None, key


async def test_no_capabilities_means_no_capability_sensors(hass: HomeAssistant, agent: FakeAgent) -> None:
    full_host(agent, capabilities=[])
    entry = await add_host(hass, agent)
    for keys in CAPABILITY_KEYS.values():
        for key in keys:
            assert registered(hass, entry.unique_id, key) is None, key


async def test_default_off_sensors(hass: HomeAssistant, agent: FakeAgent) -> None:
    full_host(agent)
    entry = await add_host(hass, agent)
    for key in ALWAYS_KEYS + [key for keys in CAPABILITY_KEYS.values() for key in keys]:
        entry_ = registered(hass, entry.unique_id, key)
        assert entry_ is not None, key
        disabled = entry_.disabled_by is er.RegistryEntryDisabler.INTEGRATION
        assert disabled == (key in DEFAULT_OFF), key
    for key in ("environment", "kernel", "protocol_version", "last_seen", "package_list_refreshed"):
        assert registered(hass, entry.unique_id, key).entity_category == "diagnostic", key


async def test_diagnostic_sensors(hass: HomeAssistant, agent: FakeAgent) -> None:
    enable_before_adding(hass, agent, "environment", "kernel", "protocol_version", "last_seen")
    before = dt_util.utcnow()
    entry = await add_host(hass, agent)

    assert state(hass, entry, "environment") == "vm"
    assert state(hass, entry, "kernel") == "6.12.48+deb13-amd64"
    assert state(hass, entry, "protocol_version") == "1.1"
    seen = dt_util.parse_datetime(state(hass, entry, "last_seen"))
    assert before - timedelta(seconds=1) <= seen <= dt_util.utcnow()

    # Offline: the other sensors are unavailable, but Last seen still shows when.
    await agent.stop()
    await wait_for(lambda: state(hass, entry, "host_status") == "offline")
    assert state(hass, entry, "kernel") == STATE_UNAVAILABLE
    assert dt_util.parse_datetime(state(hass, entry, "last_seen")) >= seen


async def test_reboot_required_unknown(hass: HomeAssistant, agent: FakeAgent) -> None:
    entry = await add_host(hass, agent)
    assert state(hass, entry, "reboot_required") == "no"
    await agent.send_groups(protocol.Groups(flags=flags("unknown")))
    await wait_for(lambda: state(hass, entry, "reboot_required") == STATE_UNKNOWN)


async def test_new_and_removed_mounts(hass: HomeAssistant, agent: FakeAgent) -> None:
    """A delta group is complete: a new mount adds sensors; a missing one makes them unavailable."""
    full_host(agent)
    entry = await add_host(hass, agent)
    data = protocol.Mount(mount="/srv/data", used_percent=10.0, free_bytes=1, total_bytes=2)

    await agent.send_groups(protocol.Groups(disks=protocol.Disks(mounts=[ROOT, data])))
    await wait_for(lambda: state(hass, entry, "disk_used_/srv/data") == "10.0")

    await agent.send_groups(protocol.Groups(disks=protocol.Disks(mounts=[data])))
    await wait_for(lambda: state(hass, entry, "disk_used_/") == STATE_UNAVAILABLE)
    assert state(hass, entry, "disk_used_/srv/data") == "10.0"


async def test_device_info_follows_the_host(hass: HomeAssistant, agent: FakeAgent) -> None:
    entry = await add_host(hass, agent)
    registry = dr.async_get(hass)
    device = registry.async_get_device_by_identifier((DOMAIN, entry.unique_id), entry.entry_id)
    assert device.name == "test-host"
    assert device.model == "Debian GNU/Linux 13"
    assert device.sw_version == "0.1.0"
    assert device.hw_version == "amd64"
    assert device.manufacturer is None

    # A later hostname change is followed.
    agent.hostname = "new-name"
    await agent.send_groups(protocol.Groups(agent=agent._agent_info()))
    await wait_for(lambda: registry.async_get(device.id).name == "new-name")

    # A name the user gave is kept.
    registry.async_update_device(device.id, name_by_user="My server")
    agent.hostname = "third-name"
    await agent.send_groups(protocol.Groups(agent=agent._agent_info()))
    await wait_for(lambda: registry.async_get(device.id).name == "third-name")
    assert registry.async_get(device.id).name_by_user == "My server"


async def test_service_names_are_not_recorded(hass: HomeAssistant, agent: FakeAgent) -> None:
    """HA history keeps the count of failed services, never their names (v1 spec §7.4)."""
    full_host(agent)
    entry = await add_host(hass, agent)
    services = hass.states.get(entity_id(hass, entry, "failed_services"))
    assert services.attributes["services"] == ["a.service", "b.service"]
    # The recorder leaves out the attributes listed here when it saves a state.
    assert "services" in services.state_info["unrecorded_attributes"]
    # No other sensor carries a name in its attributes.
    for current in hass.states.async_all("sensor"):
        if current.entity_id != services.entity_id:
            assert "services" not in current.attributes


async def test_smart_sensors(hass: HomeAssistant, agent: FakeAgent) -> None:
    full_host(agent)
    entry = await add_host(hass, agent)

    # Health is a problem sensor: on means the disk is failing.
    nvme_health = binary_state(hass, entry, "disk_health_nvme0n1")
    assert nvme_health.state == "off"
    assert nvme_health.attributes["device_class"] == "problem"
    assert nvme_health.attributes["friendly_name"] == "test-host Disk nvme0n1 health"
    assert binary_state(hass, entry, "disk_health_sda").state == "on"

    temperature = hass.states.get(entity_id(hass, entry, "disk_temperature_sda"))
    assert float(temperature.state) == 44
    assert temperature.attributes["unit_of_measurement"] == "°C"
    assert temperature.attributes["friendly_name"] == "test-host Disk sda temperature"
    wear = hass.states.get(entity_id(hass, entry, "disk_wear_nvme0n1"))
    assert float(wear.state) == 4
    assert wear.attributes["unit_of_measurement"] == "%"
    assert wear.attributes["friendly_name"] == "test-host Disk nvme0n1 wear"
    # Wear only for SSDs.
    assert registered(hass, entry.unique_id, "disk_wear_sda") is None

    # A disk whose health is not known yet (it slept since the Agent started).
    unknown = protocol.SmartDisk(device="sdb", health=None, temperature_celsius=None, wear_percent=None)
    await agent.send_groups(protocol.Groups(smart=protocol.Smart(disks=[NVME, unknown])))
    await wait_for(lambda: registered(hass, entry.unique_id, "disk_health_sdb") is not None)
    await wait_for(lambda: binary_state(hass, entry, "disk_health_sdb").state == STATE_UNKNOWN)
    assert state(hass, entry, "disk_temperature_sdb") == STATE_UNKNOWN
    # sda is gone from the group.
    assert binary_state(hass, entry, "disk_health_sda").state == STATE_UNAVAILABLE
    assert state(hass, entry, "disk_temperature_sda") == STATE_UNAVAILABLE


async def test_container_sensors(hass: HomeAssistant, agent: FakeAgent) -> None:
    full_host(agent)
    entry = await add_host(hass, agent)

    running = hass.states.get(entity_id(hass, entry, "containers_running"))
    assert running.state == "2"
    assert running.attributes["friendly_name"] == "test-host Containers running"
    assert running.attributes["containers"] == ["jellyfin", "web"]
    assert state(hass, entry, "containers_stopped") == "1"
    unhealthy = hass.states.get(entity_id(hass, entry, "containers_unhealthy"))
    assert unhealthy.state == "1"
    assert unhealthy.attributes["containers"] == ["paperless"]

    # Unknown counts while the Host cannot read its containers.
    unknown = protocol.Containers(count=None, running=None, stopped=None, unhealthy=None, items=[])
    await agent.send_groups(protocol.Groups(containers=unknown))
    await wait_for(lambda: state(hass, entry, "containers_running") == STATE_UNKNOWN)


async def test_container_names_are_not_recorded(hass: HomeAssistant, agent: FakeAgent) -> None:
    """HA history keeps container counts, never their names (v1 spec §7.4)."""
    full_host(agent)
    entry = await add_host(hass, agent)
    for key in CAPABILITY_KEYS["containers"]:
        current = hass.states.get(entity_id(hass, entry, key))
        assert "containers" in current.attributes
        assert "containers" in current.state_info["unrecorded_attributes"], key

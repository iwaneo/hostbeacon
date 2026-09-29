"""Diagnostics for one Host, redacted (v1 spec §7.9).

Redacted everywhere, also inside longer text: the Pairing key, the Host
address and hostname, IP and MAC addresses, disk serial numbers, and user
names (Home Assistant users, and the name in a /home/<name> path).
"""

from __future__ import annotations

import dataclasses
import ipaddress
import re
from typing import Any

from homeassistant.components.diagnostics import REDACTED
from homeassistant.const import CONF_HOST
from homeassistant.core import HomeAssistant
from homeassistant.helpers.device_registry import DeviceEntry

from . import HostbeaconConfigEntry
from .const import CONF_KEY

_MACS = (
    re.compile(r"(?<![0-9A-Fa-f:-])[0-9A-Fa-f]{2}(?:([:-])[0-9A-Fa-f]{2})(?:\1[0-9A-Fa-f]{2}){4}(?![0-9A-Fa-f:-])"),
    re.compile(r"(?<![0-9A-Fa-f.])[0-9A-Fa-f]{4}\.[0-9A-Fa-f]{4}\.[0-9A-Fa-f]{4}(?![0-9A-Fa-f.])"),
    # Without separators, also in interface names made from the MAC, such as
    # enx0242ac110002. Not the last part of a UUID.
    re.compile(r"(?<![0-9A-Fa-f-])[0-9A-Fa-f]{12}(?![0-9A-Fa-f])"),
)
# A run of characters that can be an IPv6 address, with at least two colons.
# Each run is checked with ipaddress, so a time like 10:00:00 stays.
_IPV6 = re.compile(r"[0-9A-Fa-f:]*:[0-9A-Fa-f]*:[0-9A-Fa-f:.]*")
_IPV4 = re.compile(r"(?<![\d.])(?:\d{1,3}\.){3}\d{1,3}(?!\d)")
# The user name in a home directory path, and in user@host once the host is redacted.
_HOME = re.compile(r"(/home/)[^/\s]+")
_USER_AT_HOST = re.compile(rf"[\w.-]+@(?={re.escape(REDACTED)})")
# Disk names that hold the serial number: /dev/disk/by-id names and WWNs.
_SERIALS = (
    re.compile(r"\b(?:ata|nvme|scsi|usb|mmc)-[^\s/]*_[^\s/]+"),
    re.compile(r"\bvirtio-[^\s/]+"),
    re.compile(r"\b(?:wwn-0x|nvme-eui\.|eui\.)[0-9A-Fa-f]+"),
    re.compile(r"(\bserial(?:[ _]?(?:number|no))?\"?\s*[:=]\s*\"?)[^\s,;\"]+", re.IGNORECASE),
)


async def async_get_config_entry_diagnostics(hass: HomeAssistant, entry: HostbeaconConfigEntry) -> dict[str, Any]:
    """Return what helps to find a problem with one Host."""
    connection = entry.runtime_data
    reboot = connection.reboot
    data = {
        "entry": {
            "title": entry.title,
            "unique_id": entry.unique_id,
            "data": {**entry.data, CONF_KEY: REDACTED},
        },
        "connection": {
            "host_status": connection.host_status,
            "online": connection.online,
            "problem": connection.problem,
            "limited": connection.limited,
            "last_seen": connection.last_seen.isoformat() if connection.last_seen else None,
            "reboot": (
                {"started_at": reboot.started_at.isoformat(), "last_boot": reboot.last_boot} if reboot else None
            ),
            "hello": dataclasses.asdict(connection.hello) if connection.hello else None,
            "groups": dataclasses.asdict(connection.groups),
            "last_run": dataclasses.asdict(connection.last_run) if connection.last_run else None,
        },
    }
    return _redact(data, await _private_values(hass, entry))


async def async_get_device_diagnostics(
    hass: HomeAssistant, entry: HostbeaconConfigEntry, device: DeviceEntry
) -> dict[str, Any]:
    """The same as for the config entry: each Host has one device."""
    return await async_get_config_entry_diagnostics(hass, entry)


async def _private_values(hass: HomeAssistant, entry: HostbeaconConfigEntry) -> list[str]:
    """The exact texts to redact: key, address, hostnames, and Home Assistant user names."""
    connection = entry.runtime_data
    hostnames = [entry.title]
    if connection.hello is not None:
        hostnames.append(connection.hello.hostname)
    if connection.groups.agent is not None:
        hostnames.append(connection.groups.agent.hostname)
    # systemd unit names escape "-", as in mnt-nas\x2dattic.mount.
    values = [entry.data[CONF_KEY], entry.data[CONF_HOST], *hostnames]
    values.extend(hostname.replace("-", r"\x2d") for hostname in hostnames)
    for user in await hass.auth.async_get_users():
        if user.system_generated:
            continue
        values.append(user.name or "")
        values.extend(str(credentials.data.get("username") or "") for credentials in user.credentials)
    # Longest first, so a longer value is redacted before a part of it.
    return sorted({value for value in values if value}, key=len, reverse=True)


def _redact(value: Any, private: list[str]) -> Any:
    if isinstance(value, dict):
        return {_redact(key, private): _redact(item, private) for key, item in value.items()}
    if isinstance(value, list | tuple):
        return [_redact(item, private) for item in value]
    if isinstance(value, str):
        return _redact_text(value, private)
    return value


def _redact_text(text: str, private: list[str]) -> str:
    for value in private:
        text = re.sub(rf"(?<![0-9A-Za-z]){re.escape(value)}(?![0-9A-Za-z])", REDACTED, text, flags=re.IGNORECASE)
    text = _USER_AT_HOST.sub(REDACTED + "@", text)
    for pattern in _SERIALS:
        text = pattern.sub(lambda match: (match.group(1) if match.groups() else "") + REDACTED, text)
    for pattern in _MACS:
        text = pattern.sub(REDACTED, text)
    text = _IPV6.sub(_redact_ipv6_run, text)
    text = _IPV4.sub(lambda match: REDACTED if _is_address(match.group()) else match.group(), text)
    return _HOME.sub(rf"\g<1>{REDACTED}", text)


def _redact_ipv6_run(match: re.Match[str]) -> str:
    """Redact the address in a run. The run may start with a word and a colon,
    as in addr:fe80::1, and end with a colon or a full stop."""
    run = match.group()
    starts = [0] + [index + 1 for index, char in enumerate(run) if char == ":" and run[index + 1 : index + 2] != ":"]
    for start in starts:
        end = len(run.rstrip("."))
        if run[start:end].endswith(":") and not run[start:end].endswith("::"):
            end -= 1
        if _is_address(run[start:end]):
            return run[:start] + REDACTED + run[end:]
    return run


def _is_address(text: str) -> bool:
    try:
        ipaddress.ip_address(text)
    except ValueError:
        return False
    return True

"""Load test (v1 spec §4.6, §14): 100 simulated Agents at the default
intervals against a test Home Assistant in Docker. It passes when Home
Assistant's added CPU stays under 5% of one core on average.

Added CPU is the average with the Agents connected minus the average of the
same Home Assistant before they were added. Each simulated Agent sends every
group with every capability, and every value changes at each interval, so
this is the most state changes the real Agent can send.

Run from the repo root (needs Docker): uv run python -m tests.load
"""

from __future__ import annotations

import argparse
import asyncio
import dataclasses
import json
import random
import subprocess
import sys
import tempfile
import time
import uuid
from pathlib import Path

import aiohttp

from custom_components.hostbeacon import protocol

from .fake_agent import FakeAgent

REPO = Path(__file__).resolve().parent.parent
IMAGE = (
    "ghcr.io/home-assistant/home-assistant:2026.9.4"
    "@sha256:3e6710a7ab2a61311d9d899b719f6c3657791c63e8f4942cec4ebc42401d6b76"
)
CONTAINER = "hostbeacon-load-test"
CLIENT_ID = "http://127.0.0.1:8123/"
LIMIT_PERCENT = 5.0

# The Agent's default intervals for the groups that change on their own
# (v1 spec §4.6). The other groups change only when something happens on
# the Host, and the Agent sends no delta for a value that did not change.
INTERVALS = {"system": 30, "network": 30, "disks": 60, "temperatures": 60}


def significant(value: float, digits: int = 3) -> int:
    """Round as the Agent rounds rates and byte counts."""
    return int(float(f"{value:.{digits}g}"))


def changed(old: float | None, new: float) -> float:
    """new, or one more when that is the old value, so every value changes."""
    return new + 1 if new == old else new


def next_groups(group: str, groups: protocol.Groups, rng: random.Random) -> protocol.Groups:
    """The next value of one group, rounded as the Agent rounds it."""
    if group == "system":
        system = groups.system
        return protocol.Groups(
            system=dataclasses.replace(
                system,
                cpu_percent=changed(system.cpu_percent, rng.randint(0, 98)),
                memory_percent=changed(system.memory_percent, rng.randint(20, 80)),
                memory_used_bytes=significant(rng.uniform(1e9, 4e9)),
                swap_percent=changed(system.swap_percent, rng.randint(0, 20)),
                load_1=round(rng.uniform(0, 4), 2),
                load_5=round(rng.uniform(0, 4), 2),
                load_15=round(rng.uniform(0, 4), 2),
            )
        )
    if group == "network":
        interfaces = [
            dataclasses.replace(
                interface,
                rx_bytes_per_second=significant(rng.uniform(1e3, 1e7)),
                tx_bytes_per_second=significant(rng.uniform(1e3, 1e7)),
                rx_bytes_total=interface.rx_bytes_total + rng.randint(10**4, 10**8),
                tx_bytes_total=interface.tx_bytes_total + rng.randint(10**4, 10**8),
            )
            for interface in groups.network.interfaces
        ]
        return protocol.Groups(network=protocol.Network(interfaces=interfaces))
    if group == "disks":
        mounts = [
            dataclasses.replace(
                mount,
                used_percent=changed(mount.used_percent, rng.randint(10, 90)),
                free_bytes=significant(rng.uniform(0.1, 0.9) * mount.total_bytes),
            )
            for mount in groups.disks.mounts
        ]
        return protocol.Groups(disks=protocol.Disks(mounts=mounts))
    temperatures = groups.temperatures
    return protocol.Groups(
        temperatures=protocol.Temperatures(cpu_celsius=changed(temperatures.cpu_celsius, rng.randint(35, 70)))
    )


def full_groups() -> protocol.Groups:
    """Every group, from the example snapshot of a Host with every capability."""
    example = json.loads((REPO / "protocol/examples/valid/snapshot_full.json").read_text())
    return protocol.decode(json.dumps(example["message"])).groups


class Agents:
    """The simulated Agents and the deltas they send."""

    def __init__(self, directory: Path, count: int) -> None:
        groups = full_groups()
        self.agents: list[FakeAgent] = []
        for number in range(count):
            path = directory / f"agent-{number:03d}"
            path.mkdir()
            agent = FakeAgent(path)
            agent.hostname = f"load-{number:03d}"
            agent.capabilities = list(groups.agent.capabilities)
            agent.enabled_actions = list(groups.agent.enabled_actions)
            agent.system = groups.system
            agent.groups = dataclasses.replace(groups, agent=None, system=None, update_run=None)
            self.agents.append(agent)
        self.deltas = 0
        self._tasks: list[asyncio.Task[None]] = []

    async def start(self) -> None:
        for agent in self.agents:
            await agent.start()

    def send_deltas(self) -> None:
        """Send each group on its interval, each Agent from its own start time."""
        for number, agent in enumerate(self.agents):
            rng = random.Random(number)
            for group, interval in INTERVALS.items():
                self._tasks.append(asyncio.create_task(self._every(agent, group, interval, rng)))

    async def _every(self, agent: FakeAgent, group: str, interval: int, rng: random.Random) -> None:
        await asyncio.sleep(rng.uniform(0, interval))
        while True:
            groups = next_groups(group, dataclasses.replace(agent.groups, system=agent.system), rng)
            if groups.system is not None:
                agent.system = groups.system
            else:
                agent.groups = dataclasses.replace(agent.groups, **{group: getattr(groups, group)})
            await agent.send_groups(groups)
            self.deltas += 1
            await asyncio.sleep(interval)

    async def stop_deltas(self) -> None:
        for task in self._tasks:
            task.cancel()
        await asyncio.gather(*self._tasks, return_exceptions=True)
        self._tasks = []

    async def stop(self) -> None:
        await self.stop_deltas()
        for agent in self.agents:
            await agent.stop()


class HomeAssistant:
    """A test Home Assistant in Docker with the Integration from this repo."""

    def __init__(self, directory: Path, image: str) -> None:
        self.config = directory / "config"
        self.config.mkdir()
        (self.config / "configuration.yaml").write_text("default_config:\n\nlogger:\n  default: warning\n")
        self.image = image
        # On Linux, Home Assistant shares the host network, so it reaches the
        # Agents on 127.0.0.1. Docker Desktop reaches them through its host name.
        self.linux = sys.platform == "linux"
        self.agent_address = "127.0.0.1" if self.linux else "host.docker.internal"
        self.url = "http://127.0.0.1:8123"
        self.headers: dict[str, str] = {}
        self._refresh_token = ""

    def start(self) -> None:
        subprocess.run(["docker", "rm", "-f", CONTAINER], capture_output=True)
        network = ["--network", "host"] if self.linux else ["-p", "127.0.0.1:8123:8123"]
        subprocess.run(
            [
                "docker", "run", "-d", "--name", CONTAINER, *network,
                "-v", f"{self.config}:/config",
                "-v", f"{REPO / 'custom_components/hostbeacon'}:/config/custom_components/hostbeacon:ro",
                self.image,
            ],
            check=True,
            capture_output=True,
        )  # fmt: skip

    def stop(self) -> None:
        subprocess.run(["docker", "rm", "-f", CONTAINER], capture_output=True)

    def logs(self) -> str:
        logs = subprocess.run(["docker", "logs", CONTAINER], capture_output=True, text=True)
        return logs.stdout + logs.stderr

    def cpu_seconds(self) -> float:
        """CPU time the container has used, from its cgroup."""
        stat = subprocess.run(
            ["docker", "exec", CONTAINER, "cat", "/sys/fs/cgroup/cpu.stat"], check=True, capture_output=True, text=True
        ).stdout
        return int(stat.split("usage_usec ")[1].split()[0]) / 1e6

    async def measure(self, seconds: float) -> float:
        """Average CPU over seconds, in percent of one core."""
        cpu, start = self.cpu_seconds(), time.monotonic()
        await asyncio.sleep(seconds)
        return (self.cpu_seconds() - cpu) / (time.monotonic() - start) * 100

    async def wait_ready(self, session: aiohttp.ClientSession, timeout: float = 300) -> None:
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            try:
                async with session.get(f"{self.url}/api/onboarding") as response:
                    if response.status == 200:
                        return
            except aiohttp.ClientError:
                pass
            await asyncio.sleep(2)
        raise RuntimeError("Home Assistant did not start:\n" + self.logs())

    async def onboard(self, session: aiohttp.ClientSession) -> None:
        """Make the owner user and log in as it."""
        user = {"client_id": CLIENT_ID, "name": "Load", "username": "load", "password": uuid.uuid4().hex, "language": "en"}
        async with session.post(f"{self.url}/api/onboarding/users", json=user) as response:
            response.raise_for_status()
            code = (await response.json())["auth_code"]
        await self._token(session, {"grant_type": "authorization_code", "code": code, "client_id": CLIENT_ID})

    async def refresh(self, session: aiohttp.ClientSession) -> None:
        """Get a new access token; one lasts 30 minutes."""
        await self._token(
            session, {"grant_type": "refresh_token", "refresh_token": self._refresh_token, "client_id": CLIENT_ID}
        )

    async def _token(self, session: aiohttp.ClientSession, form: dict[str, str]) -> None:
        async with session.post(f"{self.url}/auth/token", data=form) as response:
            response.raise_for_status()
            tokens = await response.json()
        self._refresh_token = tokens.get("refresh_token", self._refresh_token)
        self.headers = {"Authorization": f"Bearer {tokens['access_token']}"}

    async def _post(self, session: aiohttp.ClientSession, path: str, body: dict) -> dict:
        async with session.post(f"{self.url}{path}", json=body, headers=self.headers) as response:
            response.raise_for_status()
            return await response.json()

    async def add_host(self, session: aiohttp.ClientSession, agent: FakeAgent) -> None:
        """Add the Agent by address with a Pairing code, as a user would."""
        flow = await self._post(session, "/api/config/config_entries/flow", {"handler": "hostbeacon"})
        path = f"/api/config/config_entries/flow/{flow['flow_id']}"
        step = await self._post(session, path, {"host": self.agent_address, "port": agent.port})
        if step.get("step_id") != "pair":
            raise RuntimeError(f"{agent.hostname}: the address step gave {step}")
        result = await self._post(session, path, {"code": agent.new_code()})
        if result.get("type") != "create_entry":
            raise RuntimeError(f"{agent.hostname}: the Pairing step gave {result}")

    async def entry_states(self, session: aiohttp.ClientSession) -> list[str]:
        async with session.get(
            f"{self.url}/api/config/config_entries/entry?domain=hostbeacon", headers=self.headers
        ) as response:
            response.raise_for_status()
            return [entry["state"] for entry in await response.json()]

    async def host_states(self, session: aiohttp.ClientSession) -> list[dict]:
        async with session.get(f"{self.url}/api/states", headers=self.headers) as response:
            response.raise_for_status()
            return [state for state in await response.json() if state["entity_id"].split(".")[1].startswith("load_")]


async def run(args: argparse.Namespace) -> bool:
    with tempfile.TemporaryDirectory() as temporary:
        directory = Path(temporary)
        home_assistant = HomeAssistant(directory, args.image)
        agents = Agents(directory, args.agents)
        home_assistant.start()
        try:
            async with aiohttp.ClientSession() as session:
                await home_assistant.wait_ready(session)
                await home_assistant.onboard(session)
                print(f"Home Assistant started; settling for {args.settle} s", flush=True)
                await asyncio.sleep(args.settle)
                baseline = await home_assistant.measure(args.baseline)
                print(f"Baseline, no Hosts: {baseline:.2f}% of one core over {args.baseline} s", flush=True)

                await agents.start()
                for agent in agents.agents:
                    await home_assistant.add_host(session, agent)
                agents.send_deltas()
                print(f"Added {args.agents} Hosts; settling for {args.settle} s", flush=True)
                await asyncio.sleep(args.settle)
                deltas = agents.deltas
                loaded = await home_assistant.measure(args.duration)
                deltas = agents.deltas - deltas
                # Every Host's CPU usage in Home Assistant is the last one sent.
                await agents.stop_deltas()
                await asyncio.sleep(5)
                connected = sum(agent.connected > 0 for agent in agents.agents)
                await home_assistant.refresh(session)
                states = await home_assistant.entry_states(session)
                entities = {state["entity_id"]: state["state"] for state in await home_assistant.host_states(session)}
                current = sum(
                    entities.get(f"sensor.{agent.hostname.replace('-', '_')}_cpu_usage") == f"{agent.system.cpu_percent:g}"
                    for agent in agents.agents
                )
        finally:
            await agents.stop()
            logs = home_assistant.logs()
            home_assistant.stop()

    added = loaded - baseline
    print(f"With {args.agents} Hosts: {loaded:.2f}% of one core over {args.duration} s ({deltas} deltas)")
    print(f"Added CPU: {added:.2f}% of one core (limit {LIMIT_PERCENT}%)")
    print(f"Config entries loaded: {states.count('loaded')} of {args.agents}; Host entities: {len(entities)}")
    print(f"Hosts whose CPU usage in Home Assistant is the last one sent: {current} of {args.agents}")
    errors = [line for line in logs.splitlines() if "[custom_components.hostbeacon" in line]
    for line in errors[:20]:
        print("Log:", line)
    ok = True
    if states.count("loaded") != args.agents:
        print(f"FAIL: {states.count('loaded')} of {args.agents} config entries loaded")
        ok = False
    if connected != args.agents:
        print(f"FAIL: {connected} of {args.agents} Agents connected")
        ok = False
    if current != args.agents:
        print(f"FAIL: {current} of {args.agents} Hosts show the last CPU usage sent")
        ok = False
    if errors:
        print(f"FAIL: {len(errors)} Hostbeacon errors or warnings in the Home Assistant log")
        ok = False
    if added >= LIMIT_PERCENT:
        print("FAIL: added CPU is over the limit")
        ok = False
    return ok


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("--agents", type=int, default=100, help="simulated Agents (default 100)")
    parser.add_argument("--baseline", type=int, default=300, help="seconds to measure without Hosts (default 300)")
    parser.add_argument("--duration", type=int, default=600, help="seconds to measure with Hosts (default 600)")
    parser.add_argument("--settle", type=int, default=120, help="seconds to wait before each measurement (default 120)")
    parser.add_argument("--image", default=IMAGE, help="Home Assistant image")
    sys.exit(0 if asyncio.run(run(parser.parse_args())) else 1)


if __name__ == "__main__":
    main()

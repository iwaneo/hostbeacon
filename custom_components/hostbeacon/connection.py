"""The persistent connection from Home Assistant to one Agent.

The Integration opens one WebSocket over TLS, pinned to the certificate
fingerprint saved at Pairing, and logs in with the Pairing key. The Agent
sends hello, a snapshot, and then deltas. On disconnect, the Integration
reconnects with backoff. A different certificate is refused before anything
is sent, so the key never reaches it.
"""

from __future__ import annotations

import asyncio
import base64
import dataclasses
import logging
import random
import uuid
from collections.abc import Callable
from contextlib import AbstractAsyncContextManager
from datetime import datetime, timedelta

import aiohttp
from homeassistant.util import dt as dt_util

from . import protocol
from .pairing import TIMEOUT, agent_url

_LOGGER = logging.getLogger(__name__)

BACKOFF_START = 1.0
BACKOFF_MAX = 60.0
HEARTBEAT = 30
MAX_MESSAGE_BYTES = 4 * 1024 * 1024
INTEGRATION_VERSION = "0.0.0"
# How long to wait for the Agent's action_ack.
ACK_TIMEOUT = 30
# A Host that is not back this long after an accepted Reboot is Offline.
REBOOT_TIMEOUT = timedelta(minutes=15)


class ActionError(Exception):
    """The Action request could not be sent, or the Agent did not answer it."""


@dataclasses.dataclass(frozen=True)
class Reboot:
    """An accepted Reboot: when it started, and the boot time before it."""

    started_at: datetime
    last_boot: str | None


def _hello_reply(hello: protocol.HelloRequest, host_id: str | None = None) -> protocol.HelloReply:
    return protocol.HelloReply(
        id=str(uuid.uuid4()),
        reply_to=hello.id,
        integration_version=INTEGRATION_VERSION,
        protocol_version=protocol.PROTOCOL_VERSION,
        protocol_majors=protocol.PROTOCOL_MAJORS,
        host_id=host_id,
    )


def _connect(
    session: aiohttp.ClientSession,
    host: str,
    port: int,
    fingerprint: bytes,
    key: bytes,
    heartbeat: float | None = None,
) -> AbstractAsyncContextManager[aiohttp.ClientWebSocketResponse]:
    """Open the WebSocket pinned to fingerprint and log in with key.

    A different certificate is refused before anything is sent, so the key
    never reaches it. Only the lasting connection needs a heartbeat.
    """
    return session.ws_connect(
        agent_url(host, port, "/v1/ws", scheme="wss"),
        ssl=aiohttp.Fingerprint(fingerprint),
        headers={"Authorization": "Bearer " + base64.b64encode(key).decode()},
        heartbeat=heartbeat,
        max_msg_size=MAX_MESSAGE_BYTES,
        timeout=aiohttp.ClientWSTimeout(ws_close=10),
    )


async def can_log_in(session: aiohttp.ClientSession, host: str, port: int, fingerprint: bytes, key: bytes) -> bool:
    """Whether the Agent at host and port has the pinned certificate and accepts the key."""
    try:
        async with asyncio.timeout(TIMEOUT), _connect(session, host, port, fingerprint, key):
            return True
    except (aiohttp.ClientError, OSError, TimeoutError):
        return False


async def read_run_id(session: aiohttp.ClientSession, host: str, port: int, fingerprint: bytes, key: bytes) -> str | None:
    """The run ID of the Agent at host and port, if it has the pinned certificate and accepts the key."""
    try:
        async with asyncio.timeout(TIMEOUT), _connect(session, host, port, fingerprint, key) as ws:
            async for frame in ws:
                if frame.type != aiohttp.WSMsgType.TEXT:
                    break
                try:
                    message = protocol.decode(frame.data)
                except protocol.ProtocolError:
                    continue
                if isinstance(message, protocol.HelloRequest):
                    return message.run_id
    except (aiohttp.ClientError, OSError, TimeoutError):
        pass
    return None


async def remove_pairing(session: aiohttp.ClientSession, host: str, port: int, fingerprint: bytes, key: bytes) -> bool:
    """Ask the Agent to delete this Home Assistant's Pairing.

    True when the Agent confirmed it, or no longer knows the key.
    """
    request_id = str(uuid.uuid4())
    try:
        async with asyncio.timeout(TIMEOUT), _connect(session, host, port, fingerprint, key) as ws:
            async for frame in ws:
                if frame.type != aiohttp.WSMsgType.TEXT:
                    break
                try:
                    message = protocol.decode(frame.data)
                except protocol.ProtocolError:
                    continue
                if isinstance(message, protocol.HelloRequest):
                    await ws.send_str(protocol.encode(_hello_reply(message)))
                    await ws.send_str(protocol.encode(protocol.PairingRemoveRequest(id=request_id)))
                elif isinstance(message, protocol.PairingRemoveReply) and message.reply_to == request_id:
                    return True
    except aiohttp.WSServerHandshakeError as err:
        return err.status == 401
    except (aiohttp.ClientError, OSError, TimeoutError):
        pass
    return False


class HostConnection:
    """The connection to one Agent and the latest state it sent."""

    def __init__(
        self,
        session: aiohttp.ClientSession,
        name: str,
        host: str,
        port: int,
        fingerprint: bytes,
        key: bytes,
        host_id: str | None = None,
    ) -> None:
        self._session = session
        self._name = name
        # Sent in each hello reply, so the Agent knows the Host ID.
        self._host_id = host_id
        self._address = (host, port)
        self._fingerprint = fingerprint
        self._key = key
        self._listeners: list[Callable[[], None]] = []
        # Why the Agent cannot be used: "certificate" or "key". None when it works.
        self.problem: str | None = None
        self.online = False
        # The latest hello and state groups. They stay while Offline.
        self.hello: protocol.HelloRequest | None = None
        self.groups = protocol.Groups()
        # When the Agent last sent a message, or was last connected.
        self.last_seen: datetime | None = None
        # Set after an accepted Reboot until the Agent is back with a new boot time.
        self.reboot: Reboot | None = None
        self._ws: aiohttp.ClientWebSocketResponse | None = None
        # The pending Action requests by message ID: the Action and its answer.
        self._acks: dict[str, tuple[str, asyncio.Future[protocol.ActionAck]]] = {}

    @property
    def capabilities(self) -> list[str]:
        """The data sources that work on the Host, from the latest agent group."""
        if self.groups.agent is not None:
            return self.groups.agent.capabilities
        return self.hello.capabilities if self.hello else []

    @property
    def enabled_actions(self) -> list[str]:
        """The Actions the owner enabled on the Host, from the latest agent group."""
        if self.groups.agent is not None:
            return self.groups.agent.enabled_actions
        return self.hello.enabled_actions if self.hello else []

    @property
    def host_status(self) -> str:
        """Online, Rebooting, or Offline (v1 spec §9). A timer ends Rebooting after 15 minutes."""
        if self.reboot is not None:
            return "rebooting"
        return "online" if self.online else "offline"

    def _start_reboot(self) -> None:
        """Mark the Host Rebooting after the Agent accepted a Reboot."""
        flags = self.groups.flags
        self.reboot = Reboot(dt_util.utcnow(), flags.last_boot if flags else None)
        self._changed()

    def end_reboot(self) -> None:
        """End Rebooting: the Host is back, the Reboot failed, or it took too long."""
        if self.reboot is not None:
            self.reboot = None
            self._changed()

    async def request_action(self, action: str, user: str | None) -> protocol.ActionAck:
        """Send an Action request and return the Agent's answer.

        Each request has a new Action ID. It is never sent again, so Home
        Assistant never repeats an Action ID older than 1 hour.
        """
        ws = self._ws
        if ws is None or not self.online:
            raise ActionError
        request = protocol.ActionRequest(id=str(uuid.uuid4()), action_id=str(uuid.uuid4()), action=action, user=user)
        answer = asyncio.get_running_loop().create_future()
        self._acks[request.id] = (action, answer)
        try:
            await ws.send_str(protocol.encode(request))
            async with asyncio.timeout(ACK_TIMEOUT):
                return await answer
        except (aiohttp.ClientError, ConnectionError, TimeoutError) as err:
            raise ActionError from err
        finally:
            del self._acks[request.id]

    def add_listener(self, listener: Callable[[], None]) -> Callable[[], None]:
        """Call listener on every change. Returns a function that removes it."""
        self._listeners.append(listener)
        return lambda: self._listeners.remove(listener)

    def _changed(self) -> None:
        for listener in list(self._listeners):
            listener()

    async def run(self) -> None:
        """Stay connected until cancelled."""
        backoff = BACKOFF_START
        while True:
            try:
                await self._session_once()
            except aiohttp.ServerFingerprintMismatch:
                self._report("certificate", "The certificate of %s changed. Hostbeacon refuses to connect to it.")
            except aiohttp.WSServerHandshakeError as err:
                if err.status == 401:
                    self._report("key", "%s refused the Pairing key. The Pairing may have been removed on the Host.")
                else:
                    _LOGGER.debug("Cannot connect to %s: HTTP %s", self._name, err.status)
            except (aiohttp.ClientError, OSError, TimeoutError) as err:
                _LOGGER.debug("Connection to %s ended: %s", self._name, type(err).__name__)
            if self.online:
                backoff = BACKOFF_START
                self._set_offline()
            await asyncio.sleep(backoff * random.uniform(0.8, 1.2))
            backoff = min(backoff * 2, BACKOFF_MAX)

    def _report(self, problem: str, message: str) -> None:
        """Log a problem once, not on every retry."""
        if self.problem != problem:
            self.problem = problem
            _LOGGER.error(message, self._name)
            self._changed()

    def _set_offline(self) -> None:
        self.online = False
        self.last_seen = dt_util.utcnow()
        self._changed()

    async def _session_once(self) -> None:
        async with _connect(self._session, *self._address, self._fingerprint, self._key, HEARTBEAT) as ws:
            self._ws = ws
            try:
                async for frame in ws:
                    if frame.type != aiohttp.WSMsgType.TEXT:
                        break
                    try:
                        message = protocol.decode(frame.data)
                    except protocol.ProtocolError as err:
                        _LOGGER.warning("Ignored a malformed message from %s: %s", self._name, err)
                        continue
                    self.last_seen = dt_util.utcnow()
                    reply = self._handle(message)
                    if reply is not None:
                        await ws.send_str(protocol.encode(reply))
            finally:
                self._ws = None
                for _, answer in self._acks.values():
                    if not answer.done():
                        answer.set_exception(ActionError())
        if self.online:
            _LOGGER.info("%s disconnected", self._name)

    def _handle(self, message: protocol.Message) -> protocol.Message | None:
        """Update the state from one message, and return the reply if any."""
        match message:
            case protocol.HelloRequest():
                self.hello = message
                return _hello_reply(message, self._host_id)
            case protocol.Snapshot():
                self.groups = message.groups
                self.online = True
                if self.problem is not None:
                    _LOGGER.info("%s is connected again", self._name)
                self.problem = None
                self._check_rebooted()
                self._changed()
            case protocol.Delta():
                # Each group in a delta is complete and replaces the old one.
                changed = {
                    item.name: getattr(message.groups, item.name)
                    for item in dataclasses.fields(message.groups)
                    if getattr(message.groups, item.name) is not None
                }
                self.groups = dataclasses.replace(self.groups, **changed)
                self._check_rebooted()
                self._changed()
            case protocol.ActionAck():
                action, answer = self._acks.get(message.reply_to, (None, None))
                if answer is None or answer.done():
                    return None
                # Marked here, not by the caller: a failed result may come
                # right after the ack, before the caller runs again.
                if action == "reboot" and message.status == "accepted":
                    self._start_reboot()
                answer.set_result(message)
            case protocol.ActionResult(action="reboot", result="failed"):
                _LOGGER.error("Reboot of %s failed: %s", self._name, message.error)
                self.end_reboot()
            case protocol.Unknown():
                return protocol.unsupported_reply(message, str(uuid.uuid4()))
        return None

    def _check_rebooted(self) -> None:
        """End Rebooting when the Agent reports a new boot time."""
        flags = self.groups.flags
        if self.reboot is not None and flags is not None and flags.last_boot not in (None, self.reboot.last_boot):
            self.reboot = None

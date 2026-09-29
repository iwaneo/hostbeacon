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
import logging
import random
import uuid
from collections.abc import Callable

import aiohttp

from . import protocol
from .pairing import agent_url

_LOGGER = logging.getLogger(__name__)

BACKOFF_START = 1.0
BACKOFF_MAX = 60.0
HEARTBEAT = 30
MAX_MESSAGE_BYTES = 4 * 1024 * 1024
INTEGRATION_VERSION = "0.0.0"


class HostConnection:
    """The connection to one Agent and the latest state it sent."""

    def __init__(
        self, session: aiohttp.ClientSession, name: str, host: str, port: int, fingerprint: bytes, key: bytes
    ) -> None:
        self._session = session
        self._name = name
        self._url = agent_url(host, port, "/v1/ws", scheme="wss")
        self._fingerprint = aiohttp.Fingerprint(fingerprint)
        self._authorization = "Bearer " + base64.b64encode(key).decode()
        self._listeners: list[Callable[[], None]] = []
        self._problem: str | None = None
        self.online = False
        self.system: protocol.System | None = None

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
        if self._problem != problem:
            self._problem = problem
            _LOGGER.error(message, self._name)

    def _set_offline(self) -> None:
        self.online = False
        self._changed()

    async def _session_once(self) -> None:
        async with self._session.ws_connect(
            self._url,
            ssl=self._fingerprint,
            headers={"Authorization": self._authorization},
            heartbeat=HEARTBEAT,
            max_msg_size=MAX_MESSAGE_BYTES,
            timeout=aiohttp.ClientWSTimeout(ws_close=10),
        ) as ws:
            async for frame in ws:
                if frame.type != aiohttp.WSMsgType.TEXT:
                    break
                try:
                    message = protocol.decode(frame.data)
                except protocol.ProtocolError as err:
                    _LOGGER.warning("Ignored a malformed message from %s: %s", self._name, err)
                    continue
                reply = self._handle(message)
                if reply is not None:
                    await ws.send_str(protocol.encode(reply))
        if self.online:
            _LOGGER.info("%s disconnected", self._name)

    def _handle(self, message: protocol.Message) -> protocol.Message | None:
        """Update the state from one message, and return the reply if any."""
        match message:
            case protocol.HelloRequest():
                return protocol.HelloReply(
                    id=str(uuid.uuid4()),
                    reply_to=message.id,
                    integration_version=INTEGRATION_VERSION,
                    protocol_version=protocol.PROTOCOL_VERSION,
                    protocol_majors=protocol.PROTOCOL_MAJORS,
                )
            case protocol.Snapshot():
                self.system = message.groups.system
                self.online = True
                if self._problem is not None:
                    _LOGGER.info("%s is connected again", self._name)
                self._problem = None
                self._changed()
            case protocol.Delta():
                if message.groups.system is not None:
                    self.system = message.groups.system
                    self._changed()
            case protocol.Unknown():
                return protocol.unsupported_reply(message, str(uuid.uuid4()))
        return None

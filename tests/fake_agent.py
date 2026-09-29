"""A fake Agent for Integration tests: TLS on 127.0.0.1, the Pairing step, and
the WebSocket with hello, snapshot, and delta."""

from __future__ import annotations

import asyncio
import base64
import dataclasses
import datetime
import hashlib
import hmac
import secrets
import ssl
import uuid
from pathlib import Path

from aiohttp import WSMsgType, web
from cryptography import x509
from cryptography.hazmat.primitives import hashes, serialization
from cryptography.hazmat.primitives.asymmetric import ec
from cryptography.x509.oid import NameOID

from custom_components.hostbeacon import protocol
from custom_components.hostbeacon.pairing import ALPHABET, agent_proof, code_key, home_assistant_proof


def system(
    cpu: float | None = 12.0,
    memory: float | None = 34.0,
    swap: float | None = 5.0,
    load: tuple[float | None, float | None, float | None] = (None, None, None),
) -> protocol.System:
    return protocol.System(
        cpu_percent=cpu,
        memory_percent=memory,
        memory_used_bytes=1_230_000_000,
        swap_percent=swap,
        load_1=load[0],
        load_5=load[1],
        load_15=load[2],
    )


def flags(reboot_required: str = "no", last_boot: str | None = "2026-09-21T14:13:20Z") -> protocol.Flags:
    return protocol.Flags(
        reboot_required=reboot_required,
        package_task_running=False,
        package_system_broken=False,
        package_system_fix_command=None,
        last_boot=last_boot,
    )


class FakeAgent:
    """One fake Agent. Its port stays the same across stop and start."""

    def __init__(self, directory: Path) -> None:
        self.directory = directory
        self.instance_id = str(uuid.uuid4())
        self.hostname = "test-host"
        self.system = system()
        self.capabilities: list[str] = []
        self.enabled_actions: list[str] = []
        # Every Action request, and how the Agent answers the next ones: accepted
        # when refusal is None, else refused with that reason and first result.
        self.action_requests: list[protocol.ActionRequest] = []
        self.refusal: tuple[str, protocol.ActionOutcome | None] | None = None
        self.answer_actions = True
        # A result sent right after the ack of an accepted Action.
        self.result_at_once: protocol.ActionOutcome | None = None
        self.environment: str | None = "vm"
        self.kernel: str | None = "6.12.48+deb13-amd64"
        # The groups other than agent, system, and update_run.
        self.groups = protocol.Groups(flags=flags())
        self.code: str | None = None
        self.keys: set[bytes] = set()
        # Every request the Agent got, as "METHOD path?query" plus its headers.
        self.requests: list[tuple[str, dict[str, str]]] = []
        self.port = 0
        self._sockets: set[web.WebSocketResponse] = set()
        self._runner: web.AppRunner | None = None
        self.new_certificate()

    def new_certificate(self) -> None:
        """Make a new key pair and certificate, as a reinstall would."""
        key = ec.generate_private_key(ec.SECP256R1())
        name = x509.Name([x509.NameAttribute(NameOID.COMMON_NAME, "Hostbeacon Agent")])
        now = datetime.datetime.now(datetime.UTC)
        certificate = (
            x509.CertificateBuilder()
            .subject_name(name)
            .issuer_name(name)
            .public_key(key.public_key())
            .serial_number(x509.random_serial_number())
            .not_valid_before(now - datetime.timedelta(hours=1))
            .not_valid_after(now + datetime.timedelta(days=365))
            .sign(key, hashes.SHA256())
        )
        self.fingerprint = hashlib.sha256(certificate.public_bytes(serialization.Encoding.DER)).digest()
        cert_path, key_path = self.directory / "tls.crt", self.directory / "tls.key"
        cert_path.write_bytes(certificate.public_bytes(serialization.Encoding.PEM))
        key_path.write_bytes(
            key.private_bytes(serialization.Encoding.PEM, serialization.PrivateFormat.PKCS8, serialization.NoEncryption())
        )
        self._ssl = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
        self._ssl.minimum_version = ssl.TLSVersion.TLSv1_3
        self._ssl.load_cert_chain(cert_path, key_path)

    def share_identity(self, other: FakeAgent) -> None:
        """Take the instance ID and certificate of other, as a copy of it would."""
        self.instance_id = other.instance_id
        self.fingerprint = other.fingerprint
        self._ssl = other._ssl

    def new_code(self) -> str:
        self.code = "".join(secrets.choice(ALPHABET) for _ in range(12))
        return f"{self.code[:4]}-{self.code[4:8]}-{self.code[8:]}"

    async def start(self) -> None:
        app = web.Application()
        app.router.add_post("/v1/pair", self._pair)
        app.router.add_get("/v1/ws", self._websocket)
        self._runner = web.AppRunner(app)
        await self._runner.setup()
        site = web.TCPSite(self._runner, "127.0.0.1", self.port, ssl_context=self._ssl, reuse_address=True)
        await site.start()
        self.port = site._server.sockets[0].getsockname()[1]

    async def stop(self) -> None:
        """Disconnect every Home Assistant and stop listening."""
        for socket in list(self._sockets):
            await socket.close()
        if self._runner:
            await self._runner.cleanup()
            self._runner = None

    @property
    def connected(self) -> int:
        return len(self._sockets)

    async def send_system(self, value: protocol.System) -> None:
        """Change the system group and send a delta to every connection."""
        self.system = value
        await self.send_groups(protocol.Groups(system=value))

    async def send_groups(self, groups: protocol.Groups) -> None:
        """Send a delta with these groups to every connection."""
        frame = protocol.encode(protocol.Delta(id=str(uuid.uuid4()), groups=groups))
        for socket in list(self._sockets):
            await socket.send_str(frame)

    def _agent_info(self) -> protocol.AgentInfo:
        return protocol.AgentInfo(
            hostname=self.hostname,
            agent_version="0.1.0",
            newest_agent_version=None,
            capabilities=self.capabilities,
            enabled_actions=self.enabled_actions,
        )

    def _record(self, request: web.Request) -> None:
        self.requests.append((f"{request.method} {request.path_qs}", dict(request.headers)))

    async def _pair(self, request: web.Request) -> web.Response:
        self._record(request)
        body = await request.json()
        nonce = base64.b64decode(body["nonce"])
        if self.code is None:
            return web.Response(status=403)
        key = code_key(self.code, nonce)
        if not hmac.compare_digest(base64.b64decode(body["proof"]), home_assistant_proof(key, self.fingerprint, nonce)):
            return web.Response(status=403)
        self.code = None
        new_key = secrets.token_bytes(32)
        self.keys.add(new_key)
        return web.json_response(
            {
                "instance_id": self.instance_id,
                "hostname": self.hostname,
                "key": base64.b64encode(new_key).decode(),
                "proof": base64.b64encode(agent_proof(key, self.fingerprint, nonce, new_key)).decode(),
            }
        )

    async def _websocket(self, request: web.Request) -> web.StreamResponse:
        self._record(request)
        header = request.headers.get("Authorization", "")
        try:
            key = base64.b64decode(header.removeprefix("Bearer "), validate=True)
        except ValueError:
            key = b""
        if not header.startswith("Bearer ") or key not in self.keys:
            return web.Response(status=401)
        socket = web.WebSocketResponse()
        await socket.prepare(request)
        self._sockets.add(socket)
        try:
            hello = protocol.HelloRequest(
                id="hello-1",
                protocol_version=protocol.PROTOCOL_VERSION,
                protocol_majors=protocol.PROTOCOL_MAJORS,
                instance_id=self.instance_id,
                run_id=str(uuid.uuid4()),
                copied_from=[],
                hostname=self.hostname,
                agent_version="0.1.0",
                newest_agent_version=None,
                capabilities=self.capabilities,
                enabled_actions=self.enabled_actions,
                environment=self.environment,
                distro=protocol.Distro(id="debian", name="Debian GNU/Linux", version="13"),
                architecture="amd64",
                kernel=self.kernel,
            )
            await socket.send_str(protocol.encode(hello))
            reply = await asyncio.wait_for(socket.receive(), 5)
            if reply.type != WSMsgType.TEXT or not isinstance(protocol.decode(reply.data), protocol.HelloReply):
                return socket
            await socket.send_str(protocol.encode(self._snapshot()))
            async for frame in socket:
                if frame.type != WSMsgType.TEXT:
                    continue
                request = protocol.decode(frame.data)
                if isinstance(request, protocol.PairingRemoveRequest):
                    self.keys.discard(key)
                    reply = protocol.PairingRemoveReply(id=str(uuid.uuid4()), reply_to=request.id)
                    await socket.send_str(protocol.encode(reply))
                    await socket.close()
                elif isinstance(request, protocol.ActionRequest):
                    self.action_requests.append(request)
                    if self.answer_actions:
                        ack, outcome = self._ack(request), self.result_at_once
                        await socket.send_str(protocol.encode(ack))
                        if outcome is not None and ack.status == "accepted":
                            await self.send_action_result(outcome.result, outcome.error)
        finally:
            self._sockets.discard(socket)
        return socket

    def _ack(self, request: protocol.ActionRequest) -> protocol.ActionAck:
        reason, first_result = self.refusal or (None, None)
        return protocol.ActionAck(
            id=str(uuid.uuid4()),
            reply_to=request.id,
            action_id=request.action_id,
            status="refused" if reason else "accepted",
            reason=reason,
            first_result=first_result,
        )

    async def send_action_result(self, result: str, error: str | None = None) -> None:
        """Send the result of the last Action request to every connection."""
        request = self.action_requests[-1]
        message = protocol.ActionResult(
            id=str(uuid.uuid4()), action_id=request.action_id, action=request.action, result=result, error=error
        )
        for socket in list(self._sockets):
            await socket.send_str(protocol.encode(message))

    def _snapshot(self) -> protocol.Snapshot:
        return protocol.Snapshot(
            id=str(uuid.uuid4()),
            groups=dataclasses.replace(
                self.groups,
                agent=self._agent_info(),
                system=self.system,
                update_run=protocol.UpdateRun(
                    run_id=None,
                    state="idle",
                    percent=None,
                    started_at=None,
                    finished_at=None,
                    result=None,
                    installed=None,
                    remaining=None,
                    error=None,
                    needs_manual_update=protocol.NameList(count=None, names=[]),
                ),
            ),
        )

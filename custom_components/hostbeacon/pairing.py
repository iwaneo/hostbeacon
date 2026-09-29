"""The Integration's side of the Pairing step.

See "Pairing" in protocol/README.md. Home Assistant reads the Agent's
certificate fingerprint without trusting it, then proves it knows the Pairing
code with an HMAC over that fingerprint, on a connection pinned to it. The
Agent answers with a new key and its own proof. The code never crosses the
network.
"""

from __future__ import annotations

import asyncio
import base64
import hashlib
import hmac
import json
import re
import secrets
import ssl
from dataclasses import dataclass

import aiohttp
from yarl import URL

ALPHABET = "23456789ABCDEFGHJKMNPQRSTUVWXYZ"
ITERATIONS = 100_000
SIZE = 32
TIMEOUT = 10

UUID_PATTERN = re.compile(r"[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}")

# Only for reading the fingerprint before Pairing: nothing secret is sent on
# this connection.
_UNVERIFIED = ssl.SSLContext(ssl.PROTOCOL_TLS_CLIENT)
_UNVERIFIED.check_hostname = False
_UNVERIFIED.verify_mode = ssl.CERT_NONE
_UNVERIFIED.minimum_version = ssl.TLSVersion.TLSv1_3


class CannotConnect(Exception):
    """The Agent cannot be reached."""


class InvalidCode(Exception):
    """The Agent refused the code: wrong, expired, used, or cancelled."""


class PairingFailed(Exception):
    """The Agent's answer does not prove it knows the code."""


@dataclass(frozen=True, slots=True)
class Paired:
    """The result of a Pairing step."""

    instance_id: str
    hostname: str
    key: bytes
    fingerprint: bytes


def _clean(code: str) -> str:
    return code.upper().replace("-", "").replace(" ", "")


def normalize_code(code: str) -> str | None:
    """Read a code as a person may type it. None if it cannot be a code."""
    code = _clean(code)
    if len(code) != 12 or any(char not in ALPHABET for char in code):
        return None
    return code


def code_key(code: str, nonce: bytes) -> bytes:
    """Stretch a Pairing code. Slow on purpose: run it in an executor."""
    return hashlib.pbkdf2_hmac("sha256", _clean(code).encode(), b"hostbeacon pairing v1\x00" + nonce, ITERATIONS, SIZE)


def home_assistant_proof(stretched_code: bytes, fingerprint: bytes, nonce: bytes) -> bytes:
    """What Home Assistant sends to prove it knows the code."""
    return _mac(stretched_code, b"hostbeacon pair home assistant", fingerprint, nonce)


def agent_proof(stretched_code: bytes, fingerprint: bytes, nonce: bytes, key: bytes) -> bytes:
    """What the Agent answers to prove it knows the code. It also covers the new key."""
    return _mac(stretched_code, b"hostbeacon pair agent", fingerprint, nonce, key)


def _mac(key: bytes, label: bytes, *parts: bytes) -> bytes:
    return hmac.new(key, label + b"\x00" + b"".join(parts), "sha256").digest()


def pairing_id(key: bytes) -> str:
    """The Pairing's short ID on the Host: `hostbeacon pairings` shows it."""
    return hashlib.sha256(key).hexdigest()[:8]


def agent_url(host: str, port: int, path: str, scheme: str = "https") -> URL:
    """The URL of path on the Agent. Works for IPv6 addresses too."""
    return URL.build(scheme=scheme, host=host, port=port, path=path)


async def fetch_fingerprint(host: str, port: int) -> bytes:
    """Read the SHA-256 fingerprint of the certificate the Agent shows."""
    try:
        async with asyncio.timeout(TIMEOUT):
            _, writer = await asyncio.open_connection(host, port, ssl=_UNVERIFIED)
    except (OSError, TimeoutError, ssl.SSLError) as err:
        raise CannotConnect from err
    try:
        certificate = writer.get_extra_info("ssl_object").getpeercert(binary_form=True)
    finally:
        writer.close()
    return hashlib.sha256(certificate).digest()


def pairing_name(name: str) -> str:
    """The Pairing name the Agent accepts: 1 to 64 printable characters."""
    return "".join(char for char in name if char.isprintable()).strip()[:64] or "Home Assistant"


async def pair(session: aiohttp.ClientSession, host: str, port: int, code: str, name: str) -> Paired:
    """Run the Pairing step with the Agent at host and port."""
    fingerprint = await fetch_fingerprint(host, port)
    nonce = secrets.token_bytes(SIZE)
    stretched = await asyncio.get_running_loop().run_in_executor(None, code_key, code, nonce)
    body = {
        "name": pairing_name(name),
        "nonce": base64.b64encode(nonce).decode(),
        "proof": base64.b64encode(home_assistant_proof(stretched, fingerprint, nonce)).decode(),
    }
    try:
        async with session.post(
            agent_url(host, port, "/v1/pair"),
            json=body,
            ssl=aiohttp.Fingerprint(fingerprint),
            timeout=aiohttp.ClientTimeout(total=TIMEOUT),
        ) as response:
            if response.status == 403:
                raise InvalidCode
            if response.status != 200:
                raise PairingFailed(f"the Agent answered HTTP {response.status}")
            text = await response.text()
    except (aiohttp.ClientError, TimeoutError) as err:
        raise CannotConnect from err

    try:
        answer = json.loads(text)
        new_key = base64.b64decode(answer["key"], validate=True)
        proof = base64.b64decode(answer["proof"], validate=True)
        instance_id, hostname = answer["instance_id"], answer["hostname"]
    except (KeyError, TypeError, ValueError) as err:  # also JSONDecodeError
        raise PairingFailed("the Agent's answer is malformed") from err
    if not (isinstance(instance_id, str) and UUID_PATTERN.fullmatch(instance_id) and isinstance(hostname, str) and hostname):
        raise PairingFailed("the Agent's answer is malformed")
    if len(new_key) != SIZE or not hmac.compare_digest(proof, agent_proof(stretched, fingerprint, nonce, new_key)):
        raise PairingFailed("the Agent did not prove it knows the code")
    return Paired(instance_id=instance_id, hostname=hostname, key=new_key, fingerprint=fingerprint)

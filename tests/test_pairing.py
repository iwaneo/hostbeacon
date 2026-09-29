"""Tests for the Integration's side of the Pairing step."""

import json
from pathlib import Path

from custom_components.hostbeacon.pairing import (
    agent_proof,
    code_key,
    home_assistant_proof,
    normalize_code,
    pairing_id,
    pairing_name,
)

VECTOR = json.loads((Path(__file__).parent.parent / "protocol/pairing_vector.json").read_text())


def _bytes(name: str) -> bytes:
    return bytes.fromhex(VECTOR[name])


def test_pairing_vector() -> None:
    """The Integration computes the same proofs as the Agent."""
    key = code_key(VECTOR["code"], _bytes("nonce"))
    assert key == _bytes("code_key")
    assert home_assistant_proof(key, _bytes("fingerprint"), _bytes("nonce")) == _bytes("home_assistant_proof")
    assert agent_proof(key, _bytes("fingerprint"), _bytes("nonce"), _bytes("key")) == _bytes("agent_proof")


def test_code_is_read_without_dashes_spaces_or_case() -> None:
    assert normalize_code(" k7qm-4xpt 9rwd ") == "K7QM4XPT9RWD"


def test_code_with_wrong_length_or_look_alikes_is_refused() -> None:
    for code in ("K7QM-4XPT", "K7QM-4XPT-9RWD-A", "O7QM-4XPT-9RWD", "K7QM-1XPT-9RWD", "K7QM-4XPT-9RW!"):
        assert normalize_code(code) is None, code


def test_pairing_name_is_what_the_agent_accepts() -> None:
    assert pairing_name("Home") == "Home"
    assert pairing_name("") == "Home Assistant"
    assert pairing_name("\x00\n") == "Home Assistant"
    assert pairing_name("x" * 100) == "x" * 64


def test_pairing_id_matches_the_agent() -> None:
    assert pairing_id(_bytes("key")) == VECTOR["pairing_id"]

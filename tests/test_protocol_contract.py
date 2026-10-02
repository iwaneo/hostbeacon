"""Contract tests: the Integration against the shared protocol examples.

The Agent runs the same examples in agent/internal/protocol/contract_test.go.
See protocol/README.md for the example format.
"""

import json
from pathlib import Path

import pytest
from jsonschema import Draft202012Validator

from custom_components.hostbeacon import protocol

PROTOCOL_DIR = Path(__file__).parent.parent / "protocol"
SCHEMA = json.loads((PROTOCOL_DIR / "schema.json").read_text(encoding="utf-8"))
VALIDATOR = Draft202012Validator(SCHEMA)


def examples(folder: str) -> list:
    paths = sorted((PROTOCOL_DIR / "examples" / folder).glob("*.json"))
    return [
        pytest.param(json.loads(path.read_text(encoding="utf-8")), id=path.stem)
        for path in paths
    ]


def test_schema_is_valid() -> None:
    Draft202012Validator.check_schema(SCHEMA)


@pytest.mark.parametrize("example", examples("valid") + examples("unknown"))
def test_example_matches_schema(example: dict) -> None:
    VALIDATOR.validate(example["message"])
    for other in ("produced", "reply"):
        if example.get(other):
            VALIDATOR.validate(example[other])


def test_examples_cover_every_message() -> None:
    """Every message type, group, refusal reason, and Update run state has an example."""
    messages = [example.values[0]["message"] for example in examples("valid")]
    defs = SCHEMA["$defs"]

    assert {(m["type"], m["kind"]) for m in messages} == {
        ("hello", "request"),
        ("hello", "reply"),
        ("snapshot", "event"),
        ("delta", "event"),
        ("action_request", "request"),
        ("action_ack", "reply"),
        ("action_result", "event"),
        ("pairing_remove", "request"),
        ("pairing_remove", "reply"),
        ("unsupported", "reply"),
    }
    deltas = [m["groups"] for m in messages if m["type"] == "delta"]
    all_groups = [m["groups"] for m in messages if "groups" in m]
    assert set(defs["groups"]["properties"]) <= {group for groups in deltas for group in groups}
    assert set(defs["action"]["enum"]) == {m["action"] for m in messages if m["type"] == "action_request"}
    assert set(defs["refusal_reason"]["enum"]) == {
        m["reason"] for m in messages if m["type"] == "action_ack" and m["status"] == "refused"
    }
    assert set(defs["update_run_group"]["properties"]["state"]["enum"]) <= {
        groups["update_run"]["state"] for groups in all_groups if "update_run" in groups
    }


@pytest.mark.parametrize("example", examples("invalid"))
def test_invalid_example_is_rejected_by_schema(example: dict) -> None:
    assert not VALIDATOR.is_valid(example["message"]), example["description"]


@pytest.mark.parametrize("example", examples("invalid"))
def test_integration_refuses_invalid_example(example: dict) -> None:
    with pytest.raises(protocol.ProtocolError):
        protocol.decode(json.dumps(example["message"]))


def test_integration_does_not_write_what_it_would_refuse() -> None:
    with pytest.raises(protocol.ProtocolError):
        protocol.encode(protocol.Delta(id="a", groups=protocol.Groups()))


@pytest.mark.parametrize("example", examples("valid"))
def test_integration_reads_and_writes_example(example: dict) -> None:
    """Reading a message and writing it back gives the same message, minus unknown fields."""
    message = protocol.decode(json.dumps(example["message"]))

    assert json.loads(protocol.encode(message)) == example.get("produced", example["message"])


def test_integration_reads_hello_fields() -> None:
    example = json.loads((PROTOCOL_DIR / "examples/valid/hello_request.json").read_text(encoding="utf-8"))

    hello = protocol.decode(json.dumps(example["message"]))

    assert isinstance(hello, protocol.HelloRequest)
    assert hello.instance_id == "3f2b6c1e-8d4a-4b7e-9c21-5a6d7e8f9012"
    assert hello.protocol_majors == [1]
    assert hello.distro.id == "debian"
    assert hello.enabled_actions == ["reboot", "update_run", "agent_update"]


@pytest.mark.parametrize("example", examples("unknown"))
def test_unknown_type_is_ignored_and_a_request_gets_unsupported(example: dict) -> None:
    """An unknown type is not an error, so the connection stays open."""
    message = protocol.decode(json.dumps(example["message"]))

    assert isinstance(message, protocol.Unknown)
    expected = example["reply"]
    reply = protocol.unsupported_reply(message, expected["id"] if expected else "unused")
    if expected is None:
        assert reply is None
    else:
        assert json.loads(protocol.encode(reply)) == expected


@pytest.mark.parametrize(
    "case",
    [
        pytest.param(case, id=case["name"])
        for case in json.loads((PROTOCOL_DIR / "examples/malformed.json").read_text(encoding="utf-8"))
    ],
)
def test_malformed_frame_is_an_error(case: dict) -> None:
    with pytest.raises(protocol.ProtocolError):
        protocol.decode(case["frame"])


def test_protocol_version_matches_schema() -> None:
    assert protocol.PROTOCOL_VERSION == SCHEMA["x-protocol-version"] == "1.2"
    assert protocol.PROTOCOL_MAJORS == [1]

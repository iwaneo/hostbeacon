# Agent–Integration protocol

The contract between the Agent and the Integration. Rules:
[v1 spec §6](../docs/spec/v1.md#6-protocol).

- `schema.json`: one JSON Schema for every message. Protocol version
  `x-protocol-version` (major.minor, now 1.0).
- `examples/`: shared examples. The Agent (`agent/internal/protocol`) and the
  Integration (`custom_components/hostbeacon/protocol.py`) both run all of them
  in their tests.

## Example files

Each file in `valid/`, `unknown/`, and `invalid/` is one JSON object:

| Field | Meaning |
|---|---|
| `description` | What the example shows. |
| `message` | The message as sent on the WebSocket. |
| `produced` | Only when `message` has unknown fields: what a reader writes back after reading it (the same message without them). |
| `reply` | Only in `unknown/`: the expected `unsupported` reply, or `null` when there is no reply. |

What the tests check:

- `valid/`: matches the schema; both sides read it and write back the same
  message (or `produced`).
- `unknown/`: matches the schema; both sides read it as an unknown type without
  an error, so the connection stays open, and reply `unsupported` only to a
  request.
- `invalid/`: the schema rejects it.
- `malformed.json`: frames both readers must refuse with an error.

## Changing the protocol

A new minor only adds optional fields or message types. Update the schema, its
`x-protocol-version`, the examples, and both readers in the same change.

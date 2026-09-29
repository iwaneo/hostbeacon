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
- `invalid/`: the schema rejects it, and both readers refuse it.
- `malformed.json`: frames both readers must refuse with an error.

Both writers refuse to write a message their own reader would refuse.

## Connecting and Pairing

The Agent listens on one TCP port (default `8743`) with TLS 1.3 and its own
self-signed certificate. Home Assistant pins the SHA-256 fingerprint of that
certificate (of its DER bytes) at Pairing. Only the two requests below are
answered; anything else gets 404 or 405, and any request with an `Origin`
header gets 403.

### Pairing: `POST /v1/pair`

Answered only while a Pairing code is active (`hostbeacon pair`).

1. Home Assistant reads the certificate fingerprint `fp` without trusting it,
   then sends the request on a connection pinned to `fp`.
2. Request: `{"name": <Pairing name>, "nonce": <32 random bytes>, "proof": <HA proof>}`.
   The name is 1 to 64 printable characters (else 400).
3. The Agent checks the proof against its own `fp`. Wrong: 403, and one of
   the 5 tries is used. No active code, expired, used, or cancelled: 403.
4. Right: the code is used up. Answer: `{"instance_id", "hostname", "key": <32 random bytes>, "proof": <Agent proof>}`.
   Home Assistant checks the Agent proof before it saves anything.

Bytes are base64 (standard alphabet, with padding) in JSON. The code is read
without dashes or spaces, in upper case (`K7QM4XPT9RWD`).

```
code_key    = PBKDF2-HMAC-SHA256(code, "hostbeacon pairing v1" 0x00 || nonce, 100000 iterations, 32 bytes)
HA proof    = HMAC-SHA256(code_key, "hostbeacon pair home assistant" 0x00 || fp || nonce)
Agent proof = HMAC-SHA256(code_key, "hostbeacon pair agent" 0x00 || fp || nonce || key)
```

A machine in the middle has another certificate, so the proofs it sees are
bound to the wrong `fp`, and the slow `code_key` stops offline guessing in the
10 minutes a code lives. `pairing_vector.json` is a test vector both sides
check.

The key stays pending on the Agent until its first login, and is dropped when
the code expires. The Agent keeps only a SHA-256 hash of each key.

### Login: `GET /v1/ws`

A WebSocket upgrade with `Authorization: Bearer <key in base64>`, on a
connection pinned to `fp`. An unknown key gets 401. After login the Agent
sends `hello`; Home Assistant answers; then the Agent sends a `snapshot` and
`delta` messages.

## Changing the protocol

A new minor only adds optional fields or message types. Lists of fixed values
(for example `action`, `state`, `reason`) are closed within a major: a reader
refuses a value it does not know. `capabilities` is the exception; readers
ignore unknown capabilities.

Update the schema, its `x-protocol-version`, the examples, and both readers in
the same change.

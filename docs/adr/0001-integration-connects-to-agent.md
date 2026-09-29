# The Integration connects to the Agent, with pinned TLS and code-based Pairing

The Integration opens one persistent TLS WebSocket to each Agent, and the Agent pushes its data over it; Actions travel on the same connection. We chose this over MQTT (the lnxlink pattern and its hybrids), which needs an MQTT broker that Home Assistant and every Agent can reach, shared between all Hosts unless the user writes ACLs by hand; when the MQTT integration owns the entities, it also limits the update entity to install and progress. We also chose it over the Agent connecting to Home Assistant (the mobile_app pattern), which would put a Home Assistant access token on every Host that can reach far more than that Host. Hosts are on the home network or behind a user-run VPN, so Home Assistant can always reach them. Moving to MQTT discovery later would recreate every Host's devices and entities in Home Assistant; any other transport change would need every Host's Agent and credentials migrated.

## Consequences

- Each Agent listens on one TCP port. It uses a self-signed certificate, and Home Assistant pins the certificate's fingerprint at Pairing. There is no plain-text mode.
- Pairing uses a one-time Pairing code (12 characters, 10 minutes, single use, 5 attempts) that authenticates both sides. Zeroconf discovery only offers to start Pairing and is never trusted by itself.
- One Host may be paired with several Home Assistant instances, each with its own key.

# Security and privacy

What Hostbeacon can do on a Host, what it sends, what Home Assistant keeps,
and the risks the project accepts. The full rules are in
[the v1 spec](spec/v1.md), sections 4, 5, 12, and 13.

## The two parts of the Agent

The Agent on each Host runs as two programs:

- **The network part** (`hostbeacon.service`) runs as the system user
  `hostbeacon`. This user has no login shell and is not in the `docker`,
  `disk`, or `adm` groups. The network part holds the TLS key and the
  Pairings, and reads every message from Home Assistant.
- **The root helper** (`hostbeacon-helper.service`) runs as root and has no
  network code. It accepts requests only from the `hostbeacon` user, over a
  local socket. It does only this fixed list of jobs: read SMART, read the
  container state, read the SMBIOS UUID, Reboot, start the Update run, and
  start the Agent update.

The root helper checks every request again by itself: the Action must be
turned on in the Host config, and every guard must pass (no Reboot within 10
minutes after boot, one package task at a time, and so on). So a bug in the
network part can only ask for what the owner turned on. There is no shell
anywhere, and Home Assistant can never send a command, a package name, a URL,
or code.

The Host config (`/etc/hostbeacon/config.json`), the binaries, and the systemd
units are owned by root and writable only by root. If the Agent cannot read
the Host config, it refuses every Action and accepts no connections.

## The connection

- Home Assistant opens one WebSocket over TLS 1.3 to each Agent, on TCP port
  `8743`. There is no plain-text mode and no fallback.
- Home Assistant saves the Agent's certificate fingerprint at Pairing. If the
  certificate changes later, Home Assistant refuses to connect and shows a
  repair. It never accepts a new certificate by itself.
- Pairing uses a code shown on the Host. The code never crosses the network in
  plain form, works once, for 10 minutes, and stops after 5 wrong tries.
  Pairing gives each Home Assistant its own random 256-bit key.
- The Host keeps only a hash of each key. Home Assistant keeps the key itself
  (see [What Home Assistant keeps](#what-home-assistant-keeps)).
- By default the Agent accepts connections only from private address ranges
  and loopback. The Tailscale range `100.64.0.0/10` is not allowed unless the
  owner adds Home Assistant's single address.
- Before a valid key, the Agent answers only the Pairing step, and only while
  a code is active. It limits connections and messages.
- Neither side logs a key or a Pairing code, or puts one in a URL.

## Who may use an Action

Each Action (Reboot, Update run, Agent update) is off until the owner turns it
on with `sudo hostbeacon setup`. Home Assistant can see which Actions are on,
but never change them.

In Home Assistant, only admins can use an Action. Calls without a user
(automations, some voice assistants) are allowed, as in Home Assistant itself.

The root helper writes every Action request to the **Action log**: the system
journal and its own file in `/var/log/hostbeacon/` (kept 1 year): time, Action, Pairing, the Home
Assistant user name, accepted or refused, and the result. Home Assistant
cannot change or turn off the Action log. If it cannot be written, the Action
is refused.

## What is sent

Only paired Home Assistants get Host data from the Agent. The Agent sends:

- instance ID, run ID (new at each Agent start), and the list of identities
  it was copied from;
- hostname, Agent version, distro, architecture, kernel, environment (bare
  metal, VM, LXC), the capabilities it found, and which Actions are on;
- CPU usage, memory and swap, load, last boot time, CPU temperature;
- disk space per mount (with the mount path), network traffic per interface
  (with the interface name);
- SMART per disk: device name (such as `sda`), health, temperature, wear;
- failed systemd services (count and names), containers (names and states);
- Available updates: count, a capped list of package names and versions, and
  the time the package list was refreshed;
- the Update run record, Reboot required, and whether the package system is
  broken (with the command to fix it).

Home Assistant sends the Agent its version, the Host ID, and each Action
request with the Home Assistant user name (or "no HA user").

The Agent also announces itself on the local network with mDNS (zeroconf),
so Home Assistant can discover it. Anyone on that network can see the
announcement: the Host's hostname, the port, and the instance ID.

No usage statistics and no crash reports are sent anywhere. The Agent's only
other connections go to:

- the distro's package repositories (the daily package list refresh, and the
  Update run);
- GitHub Releases (the daily newest version check, and the Agent update).

## What Home Assistant keeps

- **The config entry** of each Host: address, port, certificate fingerprint,
  instance ID, and the **Pairing key**. Home Assistant stores it as plain JSON
  in `.storage/core.config_entries`, and it is part of Home Assistant backups.
  Anyone who has this file or a backup can connect to the Host as this Home
  Assistant (from an allowed address), and use the Actions that are on.
- **History** keeps counts, never lists of names. Package, service, and
  container names are shown only where Home Assistant does not record them:
  entity attributes that are not recorded, and the release notes of the
  Updates entity (read from the Host when you open the dialog, never stored).
- **Repairs** never hold a package, service, or container name.
- The start time of a Reboot, in `.storage/hostbeacon.<entry ID>`.
- **Download diagnostics** on a Host removes the Pairing key, the Host address
  and hostname, IP and MAC addresses, disk serial numbers, and user names
  (Home Assistant users, and Host users in `/home/<name>` and `<name>@<host>`),
  also inside longer text. Package names, service names, and container names
  stay in it, because they help to find a problem; check the file before you
  share it.

## Accepted risks

- **A hacked Home Assistant** can do on each Host what the owner turned on,
  within the guards: reboot, install all Available updates, or update the
  Agent to a signed release. It cannot turn on Actions, change the Action log,
  choose packages, or send code.
- **The Home Assistant user name** in the Action log is what Home Assistant
  says. The Agent cannot check it.
- **The first install** trusts HTTPS and GitHub, unless you check the minisign
  signature by hand.
- **A taken-over GitHub account or a leaked release key** can publish a signed
  bad update. Every Host with Agent update turned on would install it. If both
  signing keys are lost, owners must reinstall the Agent by hand.
- **Home Assistant backups** hold the Pairing keys. It is not confirmed that
  every local Home Assistant backup is encrypted.
- **Clone detection is best effort.** See
  [Cloned Hosts](../README.md#cloned-hosts).

## If a key leaks

- **A Home Assistant key** (for example, from a backup): on the Host, run
  `sudo hostbeacon pairings remove <ID or name>`, then `sudo hostbeacon pair`,
  and Re-pair in Home Assistant.
- **The Agent's private key**: run `sudo hostbeacon regenerate-key`. It makes a
  new key and certificate and removes every Pairing (it lists them first).
  Every Home Assistant must Re-pair the Host.

To report a vulnerability, see [SECURITY.md](../SECURITY.md).

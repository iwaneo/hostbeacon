# Hostbeacon

Shows Linux servers in Home Assistant: health, Available updates, Update runs,
and a few controls.

Hostbeacon has two parts:

- the **Agent**, a small program installed on each Linux Host (`agent/`, Go);
- the **Integration**, a Home Assistant custom integration installed through
  HACS (`custom_components/hostbeacon/`, domain `hostbeacon`).

Home Assistant connects to each Agent over TLS on your own network. Nothing is
exposed to the internet, and there is no cloud service or broker.

More docs:

- [Security and privacy](docs/security-and-privacy.md)
- [Troubleshooting](docs/troubleshooting.md)
- Design: [the v1 spec](docs/spec/v1.md) and
  [ADR 0001](docs/adr/0001-integration-connects-to-agent.md)

## Supported Hosts

| | Support |
|---|---|
| Debian, Ubuntu (apt); Fedora, RHEL, AlmaLinux, Rocky (dnf); Proxmox VE, Raspberry Pi OS | Full: sensors, Available updates, Update run, Actions |
| Other distros with systemd (Arch, openSUSE, …) | Partial, no promise: the Agent installs from the tarball; sensors and Reboot work; Available updates and Update run show "not supported" |
| Read-only or image-based (Fedora Atomic/CoreOS, MicroOS) | Like partial. Not supported at all if `/usr/local` is not writable (not checked) |
| Distro versions | Only releases that still get security updates from their vendor |
| systemd | Required. The package does not install without it |
| CPU | x86-64 (amd64) and ARM64 (aarch64) |
| Bare metal, VM | Full |
| LXC container | Supported. Load, CPU temperature, and SMART are hidden. Reboot restarts only the container |

Not supported: Windows, macOS, Linux without systemd (Alpine, Void), 32-bit
ARM, and the Agent inside a Docker container.

Hosts must be on Home Assistant's local network, or reachable over a VPN that
you run (both ways). Hostbeacon does not set up the VPN.

## Install

### 1. Install the Integration in Home Assistant

1. In HACS, open the menu (⋮) > **Custom repositories**.
2. Add `https://github.com/iwaneo/hostbeacon` with the type **Integration**.
3. Download **Hostbeacon**, then restart Home Assistant.

### 2. Install the Agent on the Host

Download the files for your Host from
[GitHub Releases](https://github.com/iwaneo/hostbeacon/releases): the `.deb`
(Debian, Ubuntu) or `.rpm` (Fedora, RHEL family) for your CPU, plus
`SHA256SUMS` and `SHA256SUMS.minisig`.

Check the download. The first command checks the file hash. The second
(optional) checks that the project signed the release; it needs
[minisign](https://jedisct1.github.io/minisign/):

```sh
sha256sum -c --ignore-missing SHA256SUMS
minisign -Vm SHA256SUMS -P RWQdtGLWn0hUvBq2UHOd68XCxhTpoXJWHiBdDrfEPGnd3lPk/v6SxsVD
```

A release may also be signed with the backup key
`RWR9UElf4XqFiZVup9OaGQM40n2/eCDXq/mnh4UBvKcgdcULHudnvmuv`.

Install the package:

```sh
sudo apt install ./hostbeacon_*.deb      # Debian, Ubuntu
sudo dnf install ./hostbeacon-*.rpm      # Fedora, RHEL family
```

On other systemd distros, unpack the tarball and run `sudo ./hostbeacon install`
in its directory (not tested). It installs into `/usr/local`.

The package asks nothing and turns on no Action. The Agent listens on TCP port
`8743`.

### 3. Run setup

```sh
sudo hostbeacon setup
```

`setup` asks:

1. which **Actions** to turn on (each one is off unless you answer yes);
2. only if Tailscale runs on the Host: Home Assistant's **VPN address**;
3. on Debian, Ubuntu, and Fedora-family Hosts: the
   [refresh time](#package-list-refresh) (press Enter to skip);
4. only if firewalld or ufw runs: whether to **open port 8743**;
5. whether to show a **Pairing code** now.

For scripted installs, give flags instead; `setup` then asks nothing and
changes only what the flags name:

```sh
sudo hostbeacon setup --actions reboot,update_run --vpn-address 100.101.102.103 --open-firewall --pair
```

The settings are saved in the Host config, `/etc/hostbeacon/config.json`. It is
owned by root; Home Assistant can read which Actions are on, but never change
them.

### 4. Pair with Home Assistant

1. On the Host, run `sudo hostbeacon pair` (or answer yes at the end of
   `setup`). It shows a Pairing code like `K7QM-4XPT-9RWD`. The code works
   once, for 10 minutes, and stops after 5 wrong tries.
2. In Home Assistant, go to **Settings > Devices & services**. The Host is
   usually listed as discovered: select **Add** and enter the code.

#### Adding a Host by address

Discovery does not work over a VPN or across networks. Then add the Host by
address: **Settings > Devices & services > Add integration > Hostbeacon**,
enter the Host's IP address or hostname (port `8743` unless you changed it),
then the Pairing code.

If the address changes later, open the Host's Hostbeacon entry and select
**Reconfigure**. Home Assistant accepts the new address only if the Agent there
has the same certificate as before.

One Host can be paired with several Home Assistants. Each needs its own code.

## Actions

An Action is something Home Assistant asks the Agent to do on the Host:

| Action | What it does in Home Assistant |
|---|---|
| `reboot` | The **Reboot** button. Refused within 10 minutes after the Host started, and while a package task runs |
| `update_run` | **Install** on the **Updates** entity: installs all Available updates. It never removes packages and never moves the Host to another distro release |
| `agent_update` | **Install** on the **Agent** entity: updates the Agent itself |

Each Action is off until you turn it on. To change them, run
`sudo hostbeacon setup` again, or `sudo hostbeacon setup --actions <list>` (the
Actions you do not name are turned off; `none` turns all off). Only Home
Assistant admins, and automations, can use an Action. The Agent writes every
request to its Action log (`/var/log/hostbeacon/` and the system journal).

## VPN address

By default the Agent accepts connections only from private address ranges
(`10.0.0.0/8`, `172.16.0.0/12`, `192.168.0.0/16`, `fc00::/7`, `fe80::/10`) and
loopback.

- If your VPN uses private addresses (for example WireGuard with
  `10.x.x.x`), you need to do nothing.
- Tailscale uses `100.64.0.0/10`, which is **not** allowed by default,
  because internet providers use that range too. Allow only Home Assistant's
  single Tailscale address:

  ```sh
  sudo hostbeacon setup --vpn-address 100.101.102.103
  ```

  Use `--vpn-address none` to remove it.

Then add the Host [by address](#adding-a-host-by-address).

## Package list refresh

On Debian, Ubuntu, and Fedora-family Hosts, the Agent refreshes the package
list (`apt-get update` or `dnf makecache`) once every 24 hours, so Home
Assistant sees new updates. Home Assistant cannot start or change it.

To refresh at a fixed hour instead, for example to line up all your Hosts, set
the **refresh time** (a whole hour, in the Host's local time):

```sh
sudo hostbeacon setup --refresh-time 03:00
```

The refresh then runs daily within 5 minutes after that hour. If the Host was
off, or another package task was running, it catches up at the next hourly
check. Use `--refresh-time none` to go back to once every 24 hours. The
**Updates** entity in Home Assistant shows the schedule in its release notes.

## Updating the Agent

- In Home Assistant: select **Install** on the Host's **Agent** entity. This
  works only when the `agent_update` Action is on.
- On the Host: `sudo hostbeacon update`.

Both download the newest release from GitHub, check its signature with the
keys built into the Agent, never install an older version, and go back to the
installed version if the new one does not start. Do not update with
`apt upgrade` or `dnf upgrade`: there is no package repository.

To update the Integration, update Hostbeacon in HACS and restart Home Assistant.

## Removing a Host

1. In Home Assistant, delete the Host's Hostbeacon entry. Home Assistant also
   removes its Pairing on the Host.
2. If the Host could not be reached, Home Assistant shows the notification
   "Host removed, but its Pairing is still on the Host" with the command to
   run on the Host, for example:

   ```sh
   sudo hostbeacon pairings list
   sudo hostbeacon pairings remove <ID or name>
   ```

To remove the Agent from the Host: `sudo apt remove hostbeacon` or
`sudo dnf remove hostbeacon`. This keeps the Agent's identity, Pairings, Host
config, and Action log, so a reinstall is the same Host.
`sudo apt purge hostbeacon` (`.deb` only) removes them too; the Host is then
new to Home Assistant.

`sudo hostbeacon pairings list` also warns about Pairings not seen for 90 days
or more.

## Cloned Hosts

Each Agent has its own identity. If you copy a Host (a cloned VM, a copied LXC
container, a restored disk image), the copy has the same identity at first.

At every start, the Agent checks `/etc/machine-id` and the SMBIOS UUID. If
they changed, it knows it is a copy: it makes a new identity and removes every
Pairing, and you add the copy as a new Host.

**Limit: clone detection is best effort.** Some copies keep the same
`machine-id` and SMBIOS UUID (for example a copied LXC container). If the
Agent does not detect the copy, and Home Assistant does not reach both at
once, Home Assistant cannot tell them apart. So run this on every
copy of a Host, before it goes online:

```sh
sudo hostbeacon reset-identity
```

If Home Assistant does reach both at once, it shows the repair
[Two machines share the identity of *Host*](docs/troubleshooting.md#two-machines-share-the-identity-of-host).

## Owner commands

Run `hostbeacon` with no arguments for the full list. The main ones (all as
root): `setup`, `pair`, `pairings list`, `pairings remove`, `status`,
`update`, `reset-identity`, `keep-identity`, `regenerate-key`.
`sudo hostbeacon status` shows whether the Agent runs, its Actions, identity,
and Pairings.

## Development

- Agent: `cd agent && go vet ./... && go test ./...`
- Integration: `uv run pytest`
- Protocol schema and shared examples: [`protocol/`](protocol/README.md)
- Load test, 100 simulated Agents (needs Docker): `uv run python -m tests.load`
- Releases: [Releasing](docs/releasing.md)

## Security

See [Security and privacy](docs/security-and-privacy.md). To report a
vulnerability, see [SECURITY.md](SECURITY.md).

## License

[MIT](LICENSE)

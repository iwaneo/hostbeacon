# Troubleshooting

Each repair that Hostbeacon shows in **Settings > Repairs** has a section
here. *Host* stands for the name of the Host in Home Assistant.

Most checks run on the Host. Start with:

```sh
sudo hostbeacon status
```

It shows whether the Agent runs, which Actions are on, the identity, and the
Pairings.

## The Host is Offline

Host status is **Offline** when Home Assistant has no working connection to
the Agent. Check, in this order:

1. The Agent runs: `systemctl status hostbeacon hostbeacon-helper`. Its log:
   `journalctl -u hostbeacon -u hostbeacon-helper`.
2. `sudo hostbeacon status` does not report a Host config error or an identity
   hold (see [Identity hold](#identity-hold)).
3. Home Assistant can reach TCP port `8743` on the Host. If firewalld or ufw
   runs, `sudo hostbeacon setup --open-firewall` opens the port.
4. Home Assistant's address is allowed. By default only private ranges are.
   Over Tailscale, set Home Assistant's address:
   `sudo hostbeacon setup --vpn-address <address>` (see
   [VPN address](../README.md#vpn-address)).
5. The address in Home Assistant is right. If it changed, select
   **Reconfigure** on the Host's entry.

If Home Assistant shows one of the repairs below, follow that section.

### Identity hold

At start, the Agent compares `/etc/machine-id` and the SMBIOS UUID with the
values it read at install. If it cannot read one of them now, it accepts no
connections until the owner decides. `sudo hostbeacon status` tells you what
to run:

- it is the same Host: `sudo hostbeacon keep-identity`;
- the value is gone for good: `sudo hostbeacon keep-identity --drop-missing`
  (clone detection is weaker after this);
- this Host is a copy of another Host: `sudo hostbeacon reset-identity`.

## Certificate of *Host* changed

**What it means:** the Agent at the Host's address shows a different
certificate than at Pairing. Home Assistant refuses to connect, so the Host is
Offline. This happens when:

- the Host was reinstalled, or the Agent was purged and installed again;
- the owner ran `sudo hostbeacon regenerate-key` or `reset-identity`;
- the Agent found that the Host is a copy, and made a new identity;
- **another machine now has the Host's address.**

**What to do:**

1. Check that the address is still the Host's address. If another machine has
   it, do not Re-pair. Fix the address with **Reconfigure** instead.
2. If the Host was reinstalled or got a new key, select the repair, then
   **Submit**. This starts Re-pair: in **Settings > Repairs**, select
   "Authentication expired for *Host*", run `sudo hostbeacon pair` on the Host,
   and enter the code.

Re-pair keeps the Host's entities, names, areas, automations, and history.

## Two machines share the identity of *Host*

**What it means:** two machines answered as the same Host at the same time.
One is probably a copy (a cloned VM, a copied LXC container) that the Agent did
not detect. Home Assistant stays connected to the saved address.

**What to do:** on the copy, run:

```sh
sudo hostbeacon reset-identity
```

Then add the copy as a new Host. The repair stays until Home Assistant
restarts, or you remove the Host. To avoid this, run `reset-identity` on every copy before it
goes online (see [Cloned Hosts](../README.md#cloned-hosts)).

## Update run on *Host* is taking over 1 hour

**What it means:** an Update run started more than 1 hour ago and still runs.
Hostbeacon never stops an Update run, because stopping the package manager in
the middle can break the package system.

**What to do:** see what it is doing:

```sh
journalctl -u hostbeacon-update-run -f
```

A large run on a slow Host can take long. The run waits up to 5 minutes for
another package tool to finish first. If it truly hangs, find the waiting
package manager process on the Host and decide yourself. The repair goes away
when the run ends.

## Package system on *Host* is broken

**What it means:** the Host's package manager reports a problem,
so an Update run may fail. The repair shows the command to run, one of:

- `sudo dpkg --configure -a` (Debian, Ubuntu: a package install was cut off);
- `sudo apt-get --fix-broken install` (Debian, Ubuntu: missing dependencies);
- `sudo dnf check` (Fedora, RHEL family: shows what is wrong).

**What to do:** run the command on the Host, and follow what it says. The
repair goes away when the Host reports that the package system works again.

## *Host* needs a manual update

**What it means:** the last Update run installed nothing, because it would
remove packages, or move the Host to another distro release. Hostbeacon never
does either by itself.

**What to do:**

1. In Home Assistant, open the Host's **Updates** entity. The release notes
   list the packages.
2. On the Host, update by hand, and read what the package manager would change
   before you accept:
   - Debian, Ubuntu: `sudo apt full-upgrade`
   - Fedora, RHEL family: `sudo dnf upgrade`
3. If a package source points at a newer distro release than the Host has, fix
   the source first.

The repair goes away when a later Update run passes its checks, or no
Available updates are left.

## Update the Agent on *Host*

**What it means:** the Agent is too old for this version of the Integration.
The Host is Offline; only the Agent update still works.

**What to do:** select **Install** on the Host's **Agent** entity (only when
Agent update is turned on), or run on the Host:

```sh
sudo hostbeacon update
```

The repair goes away when the Host is Online again.

## Update Hostbeacon in HACS

**What it means:** the Agent is newer than this version of the Integration can
use. The Host is Offline.

**What to do:** in HACS, update Hostbeacon, then restart Home Assistant. The
repair goes away when the Host is Online again.

## Other messages

### Host removed, but its Pairing is still on the Host

This notification comes when you delete a Host in Home Assistant while the
Host cannot be reached. The Host still accepts this Home Assistant's key. On
the Host, run the command from the notification:

```sh
sudo hostbeacon pairings remove <ID or name>
```

`sudo hostbeacon pairings list` shows every Pairing.

### Authentication expired for *Host*

The Agent refused Home Assistant's key, for example because its Pairing was
removed on the Host. Select it, run `sudo hostbeacon pair` on the Host, and
enter the new code.

### Agent update failed, or its result is unknown

See what happened on the Host:

```sh
journalctl -u hostbeacon-agent-update
```

If the new version did not start, the Agent went back to the version before.

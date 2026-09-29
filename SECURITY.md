# Security policy

## Reporting a vulnerability

Please do not open a public issue for a security problem.

Report it privately through GitHub:
[Report a vulnerability](https://github.com/iwaneo/hostbeacon/security/advisories/new)
(the **Security** tab, then **Report a vulnerability**).

Include what you found, how to reproduce it, and which version you used.
Do not include Pairing keys, Pairing codes, or other secrets from your own
Hosts. If you attach Home Assistant diagnostics, check the file first.

This is a hobby project maintained by one person, so replies are best effort.
A fix is released as a new version on
[GitHub Releases](https://github.com/iwaneo/hostbeacon/releases), and the
report is published as a GitHub security advisory after the release.

## Supported versions

Only the latest release gets security fixes. Update the Agent with
`sudo hostbeacon update` or the **Agent** entity in Home Assistant, and the
Integration in HACS.

## Scope

In scope:

- the Agent (installed on each Host), both the network part and the root
  helper;
- the Integration (installed in Home Assistant);
- the release files and their signatures.

What Hostbeacon promises, and the risks it accepts, are in
[Security and privacy](docs/security-and-privacy.md). A problem that is listed
there as an accepted risk is not a vulnerability, but a way to make it
smaller is welcome.

Out of scope: Home Assistant, HACS, the VPN, the distro's package manager and
repositories, and GitHub itself.

## Release signatures

`SHA256SUMS` in each release is signed with minisign. The public keys are:

- release key: `RWQdtGLWn0hUvBq2UHOd68XCxhTpoXJWHiBdDrfEPGnd3lPk/v6SxsVD`
- backup key: `RWR9UElf4XqFiZVup9OaGQM40n2/eCDXq/mnh4UBvKcgdcULHudnvmuv`

Both keys are also built into the Agent, which checks them before every Agent
update.

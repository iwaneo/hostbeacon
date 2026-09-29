# Hostbeacon

Shows Linux servers in Home Assistant: health, Available updates, Update runs,
and a few controls.

**Status: in development. Nothing is ready to install yet.**

Hostbeacon has two parts:

- the **Agent**, a small program installed on each Linux Host (`agent/`, Go);
- the **Integration**, a Home Assistant custom integration installed through
  HACS (`custom_components/hostbeacon/`, domain `hostbeacon`).

The design is in [the v1 spec](docs/spec/v1.md) and
[ADR 0001](docs/adr/0001-integration-connects-to-agent.md).

## Releases and Agent updates

Each release on [GitHub Releases](https://github.com/iwaneo/hostbeacon/releases)
has the `.deb`, `.rpm`, and tarball for amd64 and arm64, and `SHA256SUMS`
signed with [minisign](https://jedisct1.github.io/minisign/). To check a
download:

```sh
sha256sum -c --ignore-missing SHA256SUMS
minisign -Vm SHA256SUMS -P RWQdtGLWn0hUvBq2UHOd68XCxhTpoXJWHiBdDrfEPGnd3lPk/v6SxsVD
```

A release may also be signed with the backup key
`RWR9UElf4XqFiZVup9OaGQM40n2/eCDXq/mnh4UBvKcgdcULHudnvmuv`.

To update the Agent, run `sudo hostbeacon update` on the Host, or select
Install on the Host's **Agent** entity in Home Assistant (only when the owner
turned Agent update on with `sudo hostbeacon setup`). Both check the signature
with the keys built into the Agent, never install an older version, and go
back to the installed version if the new one does not start. Do not update
with `apt upgrade` or `dnf upgrade`: there is no package repository.

## Development

- Agent: `cd agent && go test ./...`
- Integration: `uv run pytest`
- Protocol schema and shared examples: [`protocol/`](protocol/README.md)

## Security

See [SECURITY.md](SECURITY.md).

## License

[MIT](LICENSE)

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

## Development

- Agent: `cd agent && go test ./...`
- Integration: `uv run pytest`
- Translations: `python3 scripts/check_translations.py`
- Protocol schema and shared examples: [`protocol/`](protocol/README.md)

## Security

See [SECURITY.md](SECURITY.md).

## License

[MIT](LICENSE)

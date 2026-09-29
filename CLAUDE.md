# CLAUDE.md

## Tickets

- Tickets for this repo live in the private planning repo `iwaneo/ha-linux`, not here. Always pass `-R iwaneo/ha-linux` to `gh issue` commands, for example `gh issue view 29 -R iwaneo/ha-linux --comments`.
- A bare ticket number like `#29` means `iwaneo/ha-linux#29`.
- The parent spec is `iwaneo/ha-linux#27`. Each ticket is a sub-issue of it; its "blocked by" links give the order. Labels: `ready-for-agent`, `ready-for-human`.
- Pull requests go to this repo, `iwaneo/hostbeacon`. In a PR body, write the ticket as `iwaneo/ha-linux#<n>`. GitHub does not close it from here; close the ticket after the PR is merged.
- This repo is public and `ha-linux` is private. Copy nothing from `ha-linux` issues into this repo except what the ticket needs (see docs/spec/v1.md §1).

## Design

- The spec is docs/spec/v1.md; decisions are in docs/adr/.
- Domain terms are defined in `CONTEXT.md` in `iwaneo/ha-linux`. Read it with `gh api repos/iwaneo/ha-linux/contents/CONTEXT.md --jq .content | base64 -d` and use its terms exactly.

## Checks

- Agent: `cd agent && go vet ./... && go test ./...`
- Integration: `uv run pytest`
- Translations: `python3 scripts/check_translations.py`
- Every change to Home Assistant text updates both `en.json` and `he.json`.

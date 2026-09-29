"""Tests for the docs: every repair has a troubleshooting section (v1 spec §15)."""

from __future__ import annotations

import json
import re
from pathlib import Path

ROOT = Path(__file__).parent.parent


def test_every_repair_has_a_troubleshooting_section() -> None:
    strings = json.loads((ROOT / "custom_components/hostbeacon/strings.json").read_text())
    troubleshooting = (ROOT / "docs/troubleshooting.md").read_text()
    headings = set(re.findall(r"^## (.+)$", troubleshooting, re.MULTILINE))

    for key, issue in strings["issues"].items():
        assert issue["title"].replace("{host}", "*Host*") in headings, key

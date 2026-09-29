"""Tests for scripts/check_translations.py."""

import json
from pathlib import Path

from scripts.check_translations import main

EN = {
    "config": {
        "step": {"user": {"title": "Add a Host", "data": {"host": "Address"}}},
        "abort": {"already_configured": "Already added"},
    }
}


def write(directory: Path, name: str, content: dict) -> None:
    (directory / name).write_text(json.dumps(content), encoding="utf-8")


def test_same_keys_pass(tmp_path: Path) -> None:
    write(tmp_path, "en.json", EN)
    write(tmp_path, "he.json", EN)

    assert main([str(tmp_path)]) == 0


def test_key_missing_in_he_fails(tmp_path: Path, capsys) -> None:
    he = json.loads(json.dumps(EN))
    del he["config"]["step"]["user"]["data"]["host"]
    write(tmp_path, "en.json", EN)
    write(tmp_path, "he.json", he)

    assert main([str(tmp_path)]) == 1
    assert "he.json is missing config.step.user.data.host" in capsys.readouterr().out


def test_extra_key_in_he_fails(tmp_path: Path, capsys) -> None:
    he = json.loads(json.dumps(EN))
    he["config"]["abort"]["unknown"] = "Unknown"
    write(tmp_path, "en.json", EN)
    write(tmp_path, "he.json", he)

    assert main([str(tmp_path)]) == 1
    assert "en.json is missing config.abort.unknown" in capsys.readouterr().out


def test_missing_file_fails(tmp_path: Path, capsys) -> None:
    write(tmp_path, "en.json", EN)

    assert main([str(tmp_path)]) == 1
    assert "he.json" in capsys.readouterr().out


def test_repo_translations_match() -> None:
    assert main([]) == 0

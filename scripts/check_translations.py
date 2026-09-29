"""Check that en.json and he.json have the same translation keys.

Usage: python3 scripts/check_translations.py [translations directory]
Exits with 1 and lists every key that is missing in one of the files.
"""

import json
import sys
from pathlib import Path

DEFAULT_DIR = Path(__file__).parent.parent / "custom_components/hostbeacon/translations"
LANGUAGES = ("en", "he")


def key_paths(data: dict, prefix: str = "") -> set[str]:
    """Return every leaf key as a dotted path, for example config.step.user.title."""
    paths = set()
    for key, value in data.items():
        path = f"{prefix}{key}"
        if isinstance(value, dict) and value:
            paths |= key_paths(value, f"{path}.")
        else:
            paths.add(path)
    return paths


def main(argv: list[str]) -> int:
    directory = Path(argv[0]) if argv else DEFAULT_DIR
    keys = {}
    for language in LANGUAGES:
        path = directory / f"{language}.json"
        if not path.is_file():
            print(f"{path.name} not found in {directory}")
            return 1
        keys[language] = key_paths(json.loads(path.read_text(encoding="utf-8")))

    problems = []
    for language in LANGUAGES:
        for other in LANGUAGES:
            for missing in sorted(keys[other] - keys[language]):
                problems.append(f"{language}.json is missing {missing}")

    for problem in problems:
        print(problem)
    if problems:
        return 1
    print(f"{' and '.join(f'{language}.json' for language in LANGUAGES)} have the same keys")
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))

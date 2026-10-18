#!/usr/bin/env python3
"""Apply project-local Pebble SDK path overrides to pebble-tool 5.0.40."""

from pathlib import Path
import sys


def replace_once(text: str, before: str, after: str, path: Path) -> str:
    count = text.count(before)
    if count != 1:
        raise SystemExit(f"Expected one pristine pebble-tool hook in {path}; found {count}")
    return text.replace(before, after, 1)


def main() -> None:
    if len(sys.argv) != 2:
        raise SystemExit("usage: patch-pebble-tool.py PEBBLE_TOOL_PACKAGE_DIR")
    package = Path(sys.argv[1])
    util_path = package / "util" / "__init__.py"
    sdk_path = package / "sdk" / "__init__.py"
    util = util_path.read_text()
    sdk = sdk_path.read_text()

    if 'os.environ.get("PEBBLE_SDK_HOME")' not in util:
        util = replace_once(
            util,
            "    if platform.system() == 'Darwin':",
            '''    if os.environ.get("PEBBLE_SDK_HOME"):
        dir = os.path.expanduser(os.environ["PEBBLE_SDK_HOME"])
    elif platform.system() == 'Darwin':''',
            util_path,
        )

    if "PEBBLE_SDK_LINK" not in sdk:
        sdk = replace_once(
            sdk,
            '        tmp_link = "/var/tmp/pebble-sdk"',
            "        tmp_link = os.environ.get('PEBBLE_SDK_LINK', '/var/tmp/pebble-sdk')",
            sdk_path,
        )

    compile(util, str(util_path), "exec")
    compile(sdk, str(sdk_path), "exec")
    util_path.write_text(util)
    sdk_path.write_text(sdk)


if __name__ == "__main__":
    main()

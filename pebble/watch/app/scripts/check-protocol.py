#!/usr/bin/env python3
import json
from pathlib import Path

root = Path(__file__).resolve().parents[1]
protocol = json.loads((root / "protocol.json").read_text())
package = json.loads((root / "package.json").read_text())
keys = protocol["keys"]
if len(keys) != len(set(keys)):
    raise SystemExit("protocol.json has duplicate AppMessage keys")
if package["pebble"]["messageKeys"] != keys:
    raise SystemExit("package.json messageKeys differ from protocol.json keys")
if len(keys) > 255:
    raise SystemExit("Pebble message key count exceeds 255")
header = root / "build/include/message_keys.auto.h"
if header.exists():
    missing = [key for key in keys if f"MESSAGE_KEY_{key}" not in header.read_text()]
    if missing:
        raise SystemExit("generated SDK message-key header missing: " + ", ".join(missing))
print(f"AppMessage protocol: {len(keys)} unique keys synchronized" + (" with generated header" if header.exists() else " (SDK-generated header not present yet)"))

#!/usr/bin/env python3
"""Check the final Emery native ELF/resource footprint against static Pebble budgets."""
import json
import os
from pathlib import Path
import re
import subprocess

watch = Path(__file__).resolve().parents[1]
repo = Path(__file__).resolve().parents[3]
build = watch / "build"
sdk = repo / ".tools/pebble-sdk/SDKs/4.33.1"
elf = build / "emery/pebble-app.elf"
resource = build / "emery/app_resources.pbpack"
pbw = build / "app.pbw"
for path in (elf, resource, pbw):
    if not path.is_file():
        raise SystemExit(f"missing final build artifact: {path}")

size_tool = sdk / "toolchain/arm-none-eabi/bin/arm-none-eabi-size"
if not size_tool.exists():
    candidates = list((sdk / "toolchain/arm-none-eabi/bin").glob("arm-none-eabi-size*"))
    size_tool = candidates[0] if candidates else size_tool
if not size_tool.is_file():
    raise SystemExit(f"missing SDK size tool: {size_tool}")
result = subprocess.run([str(size_tool), str(elf)], text=True, check=True, capture_output=True)
lines = result.stdout.strip().splitlines()
fields = lines[-1].split()
if len(fields) < 4:
    raise SystemExit(f"unrecognized size output: {result.stdout}")
text, data, bss = map(int, fields[:3])
static = text + data + bss
native_binary = os.path.getsize(build / "emery/pebble-app.bin")
resource_bytes = resource.stat().st_size
if static > 64 * 1024:
    raise SystemExit(f"static RAM {static} exceeds 64 KiB conservative budget")
if native_binary > 128 * 1024:
    raise SystemExit(f"watch binary {native_binary} exceeds 128 KiB limit")
if resource_bytes > 256 * 1024:
    raise SystemExit(f"resources {resource_bytes} exceeds 256 KiB App Store limit")

appinfo = json.loads(subprocess.check_output(["python3", "-c", "import zipfile,json,sys; print(json.dumps(json.loads(zipfile.ZipFile(sys.argv[1]).read('appinfo.json'))))", str(pbw)], text=True))
config_page = appinfo.get("configPage")
import zipfile
with zipfile.ZipFile(pbw) as archive:
    if config_page and config_page not in archive.namelist():
        raise SystemExit(f"PBW configPage missing: {config_page}")
    if archive.testzip():
        raise SystemExit("PBW archive CRC failure")
print(f"ELF .text={text} .data={data} .bss={bss}; total static RAM={static}/65536 B")
print(f"binary={native_binary}/131072 B; resources={resource_bytes}/262144 B; PBW={pbw.stat().st_size} B")
print("Budget check passed; runtime heap high-water and temporary AppMessage allocations are not measured by this static check.")

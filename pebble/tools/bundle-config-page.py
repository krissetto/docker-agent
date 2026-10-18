#!/usr/bin/env python3
"""Postprocess a Pebble PBW with a current-companion bundled config page."""

from __future__ import annotations

import argparse
import shutil
import json
from pathlib import Path, PurePosixPath
import tempfile
import urllib.parse
import zipfile


def config_entry(appinfo: dict) -> str | None:
    page = appinfo.get("configPage")
    if page is None or page == "":
        return None
    if not isinstance(page, str):
        raise ValueError("appinfo.json configPage must be a string")
    parsed = urllib.parse.urlsplit(page)
    if parsed.scheme in {"http", "https"}:
        return None
    path = PurePosixPath(page)
    if path.is_absolute() or any(part in {"", ".", ".."} for part in path.parts) or "\\" in page:
        raise ValueError(f"configPage must name a safe root-level PBW file: {page!r}")
    return path.as_posix()


def add_config_page(pbw_path: Path, app_dir: Path, config_page: str, output: Path | None = None) -> Path:
    target = output or pbw_path
    with zipfile.ZipFile(pbw_path, "r") as source:
        members = source.infolist()
        names = [member.filename for member in members]
        if len(names) != len(set(names)):
            raise ValueError("input PBW contains duplicate ZIP entries")
        if "appinfo.json" not in names:
            raise ValueError("input PBW has no root appinfo.json")
        appinfo = json.loads(source.read("appinfo.json"))
        appinfo["configPage"] = config_page
        appinfo_bytes = (json.dumps(appinfo, indent=4, ensure_ascii=False) + "\n").encode("utf-8")
        entry = config_entry(appinfo)
        if entry is None:
            raise ValueError("--config-page must be a bundled root file, not an HTTP(S) URL")
        html_path = app_dir / entry
        if not html_path.is_file():
            raise FileNotFoundError(f"appinfo configPage {entry!r} is not available at {html_path}")
        html_bytes = html_path.read_bytes()
        if not html_bytes or b"\x00" in html_bytes:
            raise ValueError("configPage must be nonempty text without NUL bytes")
        html_bytes.decode("utf-8")
        existing = source.read(entry) if entry in names else None
        if existing is not None and existing != html_bytes:
            raise ValueError(f"PBW already contains a different {entry!r} entry")
        preserved = {name: source.read(name) for name in names if name != "appinfo.json"}
        old_appinfo = source.read("appinfo.json")
        if existing == html_bytes and old_appinfo == appinfo_bytes:
            if target.resolve() != pbw_path.resolve():
                target.parent.mkdir(parents=True, exist_ok=True)
                shutil.copy2(pbw_path, target)
            return target
        new_entry = existing is None

    target.parent.mkdir(parents=True, exist_ok=True)
    with tempfile.NamedTemporaryFile(dir=target.parent, suffix=".pbw.tmp", delete=False) as temporary:
        temp_path = Path(temporary.name)
    try:
        with zipfile.ZipFile(pbw_path, "r") as source, zipfile.ZipFile(temp_path, "w") as destination:
            for member in source.infolist():
                if member.filename == "appinfo.json":
                    destination.writestr(member, appinfo_bytes)
                else:
                    destination.writestr(member, source.read(member.filename))
            if new_entry:
                config_info = zipfile.ZipInfo(entry)
                config_info.compress_type = zipfile.ZIP_DEFLATED
                config_info.external_attr = 0o100644 << 16
                destination.writestr(config_info, html_bytes)
        with zipfile.ZipFile(temp_path, "r") as verified:
            expected_names = set(names) | ({entry} if new_entry else set())
            if set(verified.namelist()) != expected_names:
                raise ValueError("PBW verification found missing or unexpected entries")
            for name, original in preserved.items():
                if verified.read(name) != original:
                    raise ValueError(f"PBW rewrite changed existing entry {name!r}")
            if verified.read("appinfo.json") != appinfo_bytes or verified.read(entry) != html_bytes:
                raise ValueError("PBW configPage verification failed")
            bad = verified.testzip()
            if bad:
                raise ValueError(f"PBW ZIP CRC verification failed for {bad!r}")
        temp_path.replace(target)
    except BaseException:
        temp_path.unlink(missing_ok=True)
        raise
    return target


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("pbw", type=Path)
    parser.add_argument("--app-dir", type=Path, required=True, help="directory against which configPage is resolved")
    parser.add_argument("--config-page", required=True, help="configPage value written to generated PBW appinfo")
    parser.add_argument("--output", type=Path, help="write elsewhere (default: atomically replace PBW)")
    args = parser.parse_args()
    output = add_config_page(
        args.pbw.resolve(), args.app_dir.resolve(), args.config_page,
        args.output.resolve() if args.output else None,
    )
    with zipfile.ZipFile(output) as archive:
        appinfo = json.loads(archive.read("appinfo.json"))
        entry = config_entry(appinfo)
        if not entry or entry not in archive.namelist():
            raise SystemExit("Generated PBW is missing its declared configPage")
        print(f"PBW configPage {entry} bundled and CRC-checked ({archive.getinfo(entry).file_size} bytes)")


if __name__ == "__main__":
    main()

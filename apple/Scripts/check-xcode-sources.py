#!/usr/bin/env python3
"""Check that both native app targets compile every DockerAgentApp Swift file once."""

from __future__ import annotations

import re
import sys
from collections import Counter, defaultdict
from pathlib import Path

OBJECT = re.compile(r"^\s*([A-F0-9]{24}) = \{(.*)\};\s*$")


def fail(messages: list[str]) -> int:
    for message in messages:
        print(f"error: {message}", file=sys.stderr)
    return 1


def main() -> int:
    apple_root = Path(sys.argv[1] if len(sys.argv) > 1 else Path(__file__).resolve().parents[1]).resolve()
    if len(sys.argv) > 2:
        return fail(["usage: check-xcode-sources.py [apple-directory]"])

    project_path = apple_root / "DockerAgent.xcodeproj" / "project.pbxproj"
    source_root = apple_root / "Sources" / "DockerAgentApp"
    if not project_path.is_file():
        return fail([f"Xcode project not found: {project_path}"])
    if not source_root.is_dir():
        return fail([f"app source directory not found: {source_root}"])

    objects: dict[str, str] = {}
    for line in project_path.read_text().splitlines():
        match = OBJECT.match(line)
        if match:
            objects[match.group(1)] = match.group(2)

    app_prefix = "Sources/DockerAgentApp/"
    file_refs: dict[str, str] = {}
    refs_by_path: dict[str, list[str]] = defaultdict(list)
    for object_id, body in objects.items():
        if "isa = PBXFileReference;" not in body:
            continue
        path_match = re.search(r'\bpath = "([^"]+)";', body)
        if path_match and path_match.group(1).startswith(app_prefix) and path_match.group(1).endswith(".swift"):
            path = path_match.group(1)
            file_refs[object_id] = path
            refs_by_path[path].append(object_id)

    build_file_refs: dict[str, str] = {}
    for object_id, body in objects.items():
        if "isa = PBXBuildFile;" not in body:
            continue
        ref_match = re.search(r"\bfileRef = ([A-F0-9]{24});", body)
        if ref_match:
            build_file_refs[object_id] = ref_match.group(1)

    phases: dict[str, list[str]] = {}
    for object_id, body in objects.items():
        if "isa = PBXSourcesBuildPhase;" not in body:
            continue
        files_match = re.search(r"\bfiles = \(([^)]*)\);", body)
        phases[object_id] = re.findall(r"[A-F0-9]{24}", files_match.group(1)) if files_match else []

    targets: dict[str, tuple[str, list[str]]] = {}
    for object_id, body in objects.items():
        if "isa = PBXNativeTarget;" not in body:
            continue
        name_match = re.search(r"\bname = ([^;]+);", body)
        phase_match = re.search(r"\bbuildPhases = \(([^)]*)\);", body)
        if name_match and phase_match:
            name = name_match.group(1).strip().strip('"')
            targets[name] = (object_id, re.findall(r"[A-F0-9]{24}", phase_match.group(1)))

    actual = {
        path.relative_to(apple_root).as_posix()
        for path in source_root.rglob("*.swift")
        if path.is_file()
    }
    errors: list[str] = []
    counts = Counter(path for path in file_refs.values())
    for path, count in sorted(counts.items()):
        if count != 1:
            errors.append(f"{path} has {count} PBXFileReference entries (expected exactly one)")
    for path in sorted(actual - set(counts)):
        errors.append(f"app Swift source is missing from the Xcode project: {path}")
    for path in sorted(set(counts) - actual):
        errors.append(f"Xcode project references a missing or non-app Swift source: {path}")

    for target_name in ("DockerAgent-iOS", "DockerAgent-macOS"):
        target = targets.get(target_name)
        if target is None:
            errors.append(f"native app target not found: {target_name}")
            continue
        _, phase_ids = target
        target_paths: list[str] = []
        for phase_id in phase_ids:
            for build_id in phases.get(phase_id, []):
                file_ref = build_file_refs.get(build_id)
                if file_ref in file_refs:
                    target_paths.append(file_refs[file_ref])
        target_counts = Counter(target_paths)
        for path in sorted(actual - set(target_counts)):
            errors.append(f"{target_name} does not compile app source: {path}")
        for path, count in sorted(target_counts.items()):
            if count != 1:
                errors.append(f"{target_name} compiles {path} {count} times (expected exactly once)")
        for path in sorted(set(target_counts) - actual):
            errors.append(f"{target_name} compiles missing/non-app source: {path}")

    if errors:
        return fail(errors)
    print(f"Xcode source membership valid: {len(actual)} app Swift files are compiled exactly once by both native app targets.")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())

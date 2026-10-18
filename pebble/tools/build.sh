#!/bin/sh
set -eu

ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd -P)
cd "$ROOT/watch/app"
"$ROOT/tools/pebble.sh" build "$@"
exec python3 "$ROOT/tools/bundle-config-page.py" "$ROOT/watch/app/build/app.pbw" --app-dir "$ROOT/watch/app" --config-page config.html

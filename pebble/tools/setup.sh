#!/bin/sh
set -eu

ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd -P)
TOOLS="$ROOT/.tools"
SDK_HOME="$TOOLS/pebble-sdk"
SDK_VERSION=4.33.1
SDK_ROOT="$SDK_HOME/SDKs/$SDK_VERSION"
CLI="$TOOLS/bin/pebble"

for command in uv python3.13 node npm; do
  if ! command -v "$command" >/dev/null 2>&1; then
    echo "Missing prerequisite: $command (see Pebble SDK requirements)." >&2
    exit 1
  fi
done

mkdir -p "$TOOLS/home" "$TOOLS/cache" "$TOOLS/tool-dir" "$TOOLS/bin"
export HOME="$TOOLS/home"
export UV_TOOL_DIR="$TOOLS/tool-dir"
export UV_TOOL_BIN_DIR="$TOOLS/bin"
export UV_CACHE_DIR="$TOOLS/cache"
export PEBBLE_SDK_HOME="$SDK_HOME"
export PEBBLE_SDK_LINK="$TOOLS/sdk-runtime-link"

if [ ! -x "$CLI" ]; then
  uv tool install --python "$(command -v python3.13)" pebble-tool==5.0.40
fi
case "$("$CLI" --version 2>/dev/null)" in
  *"Pebble Tool v5.0.40"*) ;;
  *)
    echo "Expected pebble-tool 5.0.40 at $CLI; refusing to replace another version." >&2
    exit 1
    ;;
esac

# pebble-tool 5.0.40 has no supported environment variable for either of these
# legacy global paths; patch only this project's isolated tool installation.
"$TOOLS/tool-dir/pebble-tool/bin/python" "$ROOT/tools/patch-pebble-tool.py" \
  "$TOOLS/tool-dir/pebble-tool/lib/python3.13/site-packages/pebble_tool"

if [ ! -f "$SDK_ROOT/sdk-core/manifest.json" ]; then
  "$CLI" sdk install "$SDK_VERSION"
fi
if [ ! -x "$SDK_ROOT/toolchain/arm-none-eabi/bin/arm-none-eabi-gcc" ] || \
   [ ! -x "$SDK_ROOT/toolchain/bin/qemu-pebble" ]; then
  echo "SDK $SDK_VERSION is incomplete under $SDK_ROOT; inspect the local install before retrying." >&2
  exit 1
fi

GCC_VERSION=$("$SDK_ROOT/toolchain/arm-none-eabi/bin/arm-none-eabi-gcc" --version | sed -n '1p')
case "$GCC_VERSION" in
  *"14.2.1 20241119"*) ;;
  *) echo "Unexpected ARM compiler version: $GCC_VERSION" >&2; exit 1 ;;
esac

if [ ! -e "$SDK_HOME/SDKs/current" ] || \
   [ "$(CDPATH= cd -- "$SDK_HOME/SDKs/current" 2>/dev/null && pwd -P || true)" != "$SDK_ROOT" ]; then
  "$CLI" sdk activate "$SDK_VERSION"
fi

printf 'Ready: Pebble Tool 5.0.40, SDK %s, %s\n' "$SDK_VERSION" "$GCC_VERSION"
printf 'Build with: %s/tools/build.sh -v\n' "$ROOT"

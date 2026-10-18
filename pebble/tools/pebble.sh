#!/bin/sh
set -eu

ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd -P)
TOOLS="$ROOT/.tools"
SDK_HOME="$TOOLS/pebble-sdk"
SDK_VERSION=4.33.1
SDK_ROOT="$SDK_HOME/SDKs/$SDK_VERSION"
SDK_CURRENT="$SDK_HOME/SDKs/current"
SDK_LINK="$TOOLS/sdk-runtime-link"
CLI="$TOOLS/bin/pebble"
TOOLCHAIN_BIN="$SDK_ROOT/toolchain/arm-none-eabi/bin"
QEMU_BIN="$SDK_ROOT/toolchain/bin/qemu-pebble"

if [ ! -x "$CLI" ]; then
  echo "pebble-tool 5.0.40 is missing at $CLI; install it into pebble/.tools first." >&2
  exit 1
fi
case "$("$CLI" --version 2>/dev/null)" in
  *"Pebble Tool v5.0.40"*) ;;
  *)
    echo "Expected isolated pebble-tool 5.0.40 at $CLI." >&2
    exit 1
    ;;
esac
if [ ! -f "$SDK_ROOT/sdk-core/manifest.json" ] || [ ! -x "$TOOLCHAIN_BIN/arm-none-eabi-gcc" ] || [ ! -x "$QEMU_BIN" ]; then
  echo "Pebble SDK 4.33.1 and its ARM compiler must be installed under $SDK_ROOT." >&2
  exit 1
fi

# The CLI's default SDK activation and /var/tmp link are unsafe for an isolated build.
export HOME="$TOOLS/home"
export PEBBLE_SDK_HOME="$SDK_HOME"
export PEBBLE_SDK_LINK="$SDK_LINK"
export PEBBLE_EXTRA_PATH="$SDK_ROOT/toolchain/bin:$TOOLCHAIN_BIN"
export PEBBLE_QEMU_PATH="$QEMU_BIN"
export CC="$TOOLCHAIN_BIN/arm-none-eabi-gcc"
export AR="$TOOLCHAIN_BIN/arm-none-eabi-ar"

if [ -e "$SDK_LINK" ] || [ -L "$SDK_LINK" ]; then
  if [ ! -L "$SDK_LINK" ] || [ "$(readlink "$SDK_LINK")" != "$SDK_HOME" ]; then
    echo "Refusing to replace unexpected SDK link path: $SDK_LINK" >&2
    exit 1
  fi
fi

if [ ! -e "$SDK_CURRENT" ] || [ "$(CDPATH= cd -- "$SDK_CURRENT" 2>/dev/null && pwd -P || true)" != "$SDK_ROOT" ]; then
  "$CLI" sdk activate "$SDK_VERSION"
fi

exec "$CLI" "$@"

#!/bin/sh
set -eu

ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
PROJECT="$ROOT/DockerAgent.xcodeproj"
IOS_TARGET=DockerAgent-iOS
MACOS_SCHEME=DockerAgent-macOS
DERIVED_DATA_ROOT=${DERIVED_DATA_ROOT:-"${TMPDIR:-/tmp}/DockerAgent-XcodeDerivedData"}

python3 "$ROOT/Scripts/check-xcode-sources.py" "$ROOT"
xcodebuild -list -project "$PROJECT"

build_mobile_sdk() {
  name=$1
  sdk=$2
  build_root="$DERIVED_DATA_ROOT/$name"
  if [ "$name" = ios-simulator ]; then
    xcodebuild \
      -project "$PROJECT" \
      -target "$IOS_TARGET" \
      -configuration Debug \
      -sdk "$sdk" \
      SYMROOT="$build_root/Build/Products" \
      OBJROOT="$build_root/Build/Intermediates.noindex" \
      CODE_SIGNING_ALLOWED=YES \
      CODE_SIGNING_REQUIRED=YES \
      CODE_SIGN_IDENTITY=- \
      DEVELOPMENT_TEAM=FAKETEAMID \
      ARCHS=arm64 ONLY_ACTIVE_ARCH=YES \
      build
  else
    xcodebuild \
      -project "$PROJECT" \
      -target "$IOS_TARGET" \
      -configuration Debug \
      -sdk "$sdk" \
      SYMROOT="$build_root/Build/Products" \
      OBJROOT="$build_root/Build/Intermediates.noindex" \
      CODE_SIGNING_ALLOWED=NO \
      build
  fi
}

build_mobile_sdk ios-simulator iphonesimulator
build_mobile_sdk ios iphoneos
xcodebuild \
  -project "$PROJECT" \
  -scheme "$MACOS_SCHEME" \
  -destination 'generic/platform=macOS' \
  -derivedDataPath "$DERIVED_DATA_ROOT/macos" \
  CODE_SIGNING_ALLOWED=NO \
  build

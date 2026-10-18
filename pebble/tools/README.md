# Isolated Pebble toolchain

The local SDK/toolchain install is intentionally ignored by Git. On macOS,
install the pinned toolchain under `pebble/.tools` with:

```sh
./pebble/tools/setup.sh
```

Prerequisites: `uv`, Python 3.13, Node, and npm already installed. Setup pins
`pebble-tool==5.0.40`, SDK core 4.33.1, the SDK's host-architecture compiler
bundle, and checks that the ARM compiler is xPack GCC 14.2.1. It puts uv's tool
environment/bin/cache, SDK, home directory, and Pebble legacy SDK link under
`pebble/.tools`; it does not modify global package state or
`/var/tmp/pebble-sdk`. The official SDK installer prints its legal notice but
requires no interactive response. No Docker or elevated privileges are used.

Build the Emery/Pebble Time 2 app from the repository root:

```sh
./pebble/tools/build.sh -v
```

Other commands go through the project wrapper from the app directory. For an
emulator smoke test (macOS ARM64 verified):

```sh
cd pebble/watch/app
../../tools/pebble.sh install --emulator emery
../../tools/pebble.sh screenshot /tmp/pebble-emery.png --emulator emery --no-open
../../tools/pebble.sh kill
```

The SDK bundles patched `qemu-pebble` (`10.1.5-pebble14` on the verified host)
and an Emery firmware image; this exercises the app in an Emery emulator, not
on a real watch. The QEMU image has no microphone, and phone/network, BLE,
touch, speaker, power and firmware behavior remain emulator-dependent; use
physical hardware and a compatible phone app for those acceptance checks. The
CLI emulator is persistent until killed; use the final command above to stop
it.

Compiler note: Waf's probe first resolves the compiler by name from `PATH`,
then checks the compiler name again using its original environment. The
isolated shell starts without the SDK toolchain on `PATH`, so Waf can find the
compiler during `find_program` yet fail its `check_exe` step. The wrapper passes
absolute `CC` and `AR` paths to avoid that second lookup and confines all SDK
paths to this project.

Baseline from 2026-10-18 before the C/JS changes then in progress: full Emery
build succeeded with only `app.c:138` warning that static `action_selected`
was declared but not defined (plus the linker RWX-segment warning). At that
snapshot the firmware binary was 8,718 bytes, PBW 65,856 bytes, and GNU `size`
reported `.text=8,696`, `.data=22`, `.bss=2,576` bytes, totaling 11,294 bytes
text/data/bss; the SDK reported 11,294 bytes total app RAM footprint and
119,778 bytes heap. These sizes are time-specific and must be regenerated after
app changes. The Emery QEMU install and screenshot succeeded; screenshot was
saved to `/tmp/pebble-emery.png` (200×228 RGBA). No physical device was tested.

## Native AppMessage fixture smoke

To exercise the real C decoder/framing state machine in Emery without a runtime,
credentials, or C/JS test mocks:

```sh
./pebble/tools/phone-fixture-smoke.py
```

The harness verifies source/PBW freshness, installs a temporary native-only PBW
(no bundled PKJS executable) into the official Emery QEMU, then sends actual
Pebble `AppMessage` dictionaries through the Pebble Tool QEMU transport. The
fixture sends runtimes, exact source/root-agent choices, sessions, and a
5-row chat with review/attention metadata; it validates AppMessage transport
ACKs and the watch app's type/request_id/frame/index ACK on every ordered frame.
It also sends a skipped-index terminal frame and checks the native app ignores
it. Seven 200×228 screenshots (including chat, review and status/heap) are written
to `/tmp/pebble-native-smoke`; the emulator is stopped on completion. Fixtures
are explicit and credential-free; none are app startup defaults, and no
backend/network request is made. `--list` reports exact encoded sizes. Dictation,
microphone, provider/network behavior, speaker audio, and physical-device
behavior are not testable in this fixture-only QEMU session; voice is not invoked.

The build wrapper adds `config.html` as a root PBW entry and sets generated
PBW `appinfo.json.configPage`. This is needed because the SDK 4.33.1 Waf builder
omits that field from its input schema/archive. The current public mobile-app
source loader resolves that exact appinfo filename via a PBW root ZIP entry and
converts it into a `data:text/html` URL; the ZIP CRC is validated. The config
bridge still requires the companion's plugins setting enabled and a compatible
released mobile app; the page and PKJS code must feature-probe before enabling
experimental provisioning. No build/test claims imply that a signed App Store
companion accepts or exposes this source-mirror API.

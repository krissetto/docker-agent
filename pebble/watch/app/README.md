# docker-agent Pebble Time 2 watchapp

Emery-only native C watchapp and PebbleKit JS phone bridge. The root project
README covers runtime/auth limits, bounded polling and resources. The
phone/watch wire contract is [`protocol.json`](protocol.json), and platform
API evidence is [`../../docs/platform-evidence.md`](../../docs/platform-evidence.md).

From repository root:

```sh
./pebble/tools/setup.sh
./pebble/tools/build.sh -v
node --check pebble/watch/app/src/pkjs/index.js
node pebble/watch/app/tests/phone.test.js
node pebble/watch/app/tests/config-page.test.js
python3 pebble/watch/app/scripts/check-protocol.py
python3 pebble/watch/app/scripts/check-budget.py
```

The SDK generates `build/include/message_keys.auto.h` from the exact
`protocol.json` key list mirrored by `package.json`. `scripts/embed-config.js`
builds the source-controlled config HTML into PKJS, and `pebble/tools/build.sh`
postprocesses/CRC-validates the PBW config page because the upstream 4.33.1
SDK packager ignores `configPage`. `check-budget.py` reports final ELF static
sizes/native binary/resources/PBW and enforces conservative limits; it cannot
measure runtime heap high-water or peak AppMessage allocations. The host Emery
QEMU smoke commands are in `pebble/tools/README.md`; they are not real-watch,
BLE, speech-provider or physical audio acceptance tests.

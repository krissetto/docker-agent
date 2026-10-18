# docker-agent for Pebble Time 2

This directory builds an Emery-only native C watchapp and its PebbleKit JS
bridge for the official Pebble phone companion. The watch has no Internet
connection. The phone performs HTTP, chooses the configured source/root pair,
and sends only bounded display projections to the watch. The app displays a
small recent chat and runtime/queue/attention status; approvals and questions
are shown read-only for answering on the phone or desktop. Replies and canned
responses use an explicit review/Send step. Voice uses the public
`DictationSession` API with system confirmation left on, then requires app
Review and explicit Send; the transcription cannot be character-edited on the
watch (re-record to change it).

## Runtime and product limits

- Runtime choices are explicit. Each runtime is independently configured with
  a name, HTTPS URL, auth mode, exact **source ID**, and exact
  `root_agent_name` returned from `GET /api/agents` and explicitly selected in phone settings. These identities are not
  interchangeable (`/api/agents[].name` is the source key). A new chat sends
  source and exact root agent to `/api/v2/sessions/start`; the watch does not
  synthesize a successful session before the server confirms it. Existing
  chat sends only after `recent-chat` confirms `session_id == root_session_id`
  and `execution_available=true`.
- The additive `GET /api/v2/sessions/{id}/recent-chat?limit=5&cursor=…` route
  is read-only and non-restoring. It contains at most five recent visible
  messages for the watch; `before_cursor` is opaque. A stale-cursor HTTP 409
  shows “older history expired · retry from newest chat”, not an automatic
  retry loop. The phone rejects oversized response bodies before JSON parsing
  and preserves decimal int64 message IDs without converting them to JS
  numbers.
- The C app stages begin/meta/row/end frames and publishes only a complete
  ordered view. Its watch-reported inbox size is negotiated per request; the
  bridge supports 1 KiB minimum and 4 KiB maximum frames. One physical frame is
  in flight at a time, ACKed by request ID/frame/index; bounded retransmission
  is only for frame delivery, never for a POST. If the link cannot deliver a
  complete view, the prior view is not partly overwritten.
- The watch stores at most 10 visible transcript rows, bounded draft/reader
  text, and fixed session/runtime lists. It does not hold a full transcript,
  audio, or an offline send queue. Polling is foreground-only, driven by the
  watch, finite and backoff-limited; there are no background alerts.
- `pending` means queued user input, not attention. Approval/elicitation counts
  are used only when the resident projection says `attention_known=true`; cold
  or historical attention remains unknown. The watch does not answer approvals.
- Sound is an explicit nonsecret phone preference: Haptic by default,
  Sound + haptic, or Silent. The C app respects system mute and Quiet Time.
- Dictation requires the user's supported Pebble companion speech provider
  and Internet; it may be disabled. The UI reports provider/connectivity
  failures and keeps canned compose available. No raw microphone capture or
  new speech backend is added.

## Runtime setup and auth constraints

The settings page is bundled into the PBW and loads a user-owned
name/URL/auth/source/root selection and alert preference. The bundled config
page loader is evidenced in current companion source only when plugins are
enabled; the signed shipping iOS app build was not identified or tested. The
legacy `showConfiguration` → `data:text/html` path and secret-free
`pebblejs://close` settings fallback are DOM/source-tested, but actual legacy
settings launch on the signed iOS companion is also unverified. Consequently
consumer No-sign-in runtime setup on a retail phone remains an acceptance gate,
separate from the bearer API redirect-security gate. Its source picker
uses the compact `/api/agents` fields `{name: source, root_agent_name, description}`;
server sources are never substituted with a root agent name. Private no-auth
requires an explicit trust confirmation and private/local host. Plain HTTP
requires a separate local-development opt-in. Do not expose a no-auth API
server to public Internet.

Bearer entry is intentionally present but API bearer requests are **disabled**
until the active signed phone app's redirect behavior is verified. Public
current-companion source has a config-page → PKJS `configmessage` route, but
this build cannot establish that the actual shipped phone implementation strips
`Authorization` before following a cross-origin redirect. Checking
`responseURL` happens too late to prove the header was not forwarded. No token
is persisted or put into a URL, logs, AppMessage, or watch memory. A configured
bearer runtime fails closed with a clear unsupported-transport message; it
never silently becomes unauthenticated. The source/docs gate is recorded in
`docs/platform-evidence.md`.

## Build and tests

The official `pebble-tool` 5.0.40, Pebble SDK 4.33.1 and ARM GCC 14.2.1 are
installed under ignored `pebble/.tools` via the setup script. This was built on
macOS arm64; the wrapper keeps SDK state and its compiler symlink under the
project and does not modify `/var/tmp/pebble-sdk` or global tools.

```sh
./pebble/tools/setup.sh
./pebble/tools/build.sh -v
node --check pebble/watch/app/src/pkjs/index.js
node pebble/watch/app/tests/phone.test.js
node pebble/watch/app/tests/config-page.test.js
python3 pebble/watch/app/scripts/check-protocol.py
python3 pebble/watch/app/scripts/check-budget.py
```

`protocol.json` is the transport source of truth; `package.json` message keys
are checked against it and the SDK generates
`watch/app/build/include/message_keys.auto.h`. `scripts/embed-config.js`
embeds `watch/app/config.html` into PKJS. The build wrapper postprocesses the PBW
to include/verify the companion config-page field and HTML payload, then checks
archive CRCs. The Node phone tests exercise `protocol.json` keys and the actual
PKJS command handler with mocked Pebble events/XHR: URL validation including no-URL runtime
fallback, public-private auth modes, bearer transport refusal, source/root
pairing, bounded response bodies, redirect-result checks, exact int64 IDs,
recent-chat attention/execute gates, 409 stale cursor behavior, framed ACKs,
new-start lost-response receipt idempotency, and runtime host-change invalidation.
The config-page DOM test executes the authored HTML and checks bridge probing,
root/source selection, no-auth trust, token handling, and no secret leakage.
These are mocks rather than a real API server or signed-phone companion test.

For an Emery emulator run (this is not a physical watch or companion test):

```sh
cd pebble/watch/app
../../tools/pebble.sh install --emulator emery
../../tools/pebble.sh screenshot /tmp/pebble-emery.png --emulator emery --no-open
../../tools/pebble.sh kill
```

## Resource budget

Pebble Time 2 Emery has a documented 128 KiB code+heap ceiling, not 128 KiB
free heap; the SDK reserves a 4 KiB native stack. The budget check enforces a
conservative 64 KiB static ELF design budget (text+data+bss), an independently
reported 128 KiB native binary ceiling, and the 256 KiB App Store resource cap.
Latest native build: `.text=22,578`, `.data=376`, `.bss=19,108` bytes; static
footprint 42,062 / 65,536-byte conservative guard; native binary 23,156 /
131,072-byte platform ceiling; 4,092 / 262,144-byte resources; SDK reports
89,010 bytes build-time heap available (peak/high-water heap and AppMessage
temporary memory not measured). Final PBW: 191,703 bytes including the
26,264-byte config page and 70,065-byte combined PKJS bundle. QEMU does not
validate retail companion runtime, speech service, bearer redirect behavior,
BLE, physical speaker/haptics, battery, or real-device firmware.

## Emulator integration status

The isolated native fixture harness passed in Emery QEMU using synthetic AppMessage
dictionaries: runtime → exact source/root selection → sessions → button-selected
`session-7a` (watch emitted the type-4 `open_session` command) → bounded chat.
Ordered frames were ACKed, a duplicate begin was re-ACKed without duplicate
publication, and an out-of-order terminal frame was ignored. This classifies the
core open-session command/navigation boundary as working under the fixture; the
harness then injects the selected chat response rather than connecting a live
API. Captures and exact fixtures are saved under
`docs/assets/emulator/` (these are actual Emery QEMU screenshots, not UX mockups).
The emulator stopped after the run. No API server, retail phone companion, speech
provider, bearer auth, or physical watch was connected. The SDK wallpaper/mockups under
`docs/assets/screens.png` are designer mockups, not app captures.

# Native Apple app build

`DockerAgent.xcodeproj` contains native SwiftUI app targets for iPhone/iPad
and macOS 26 (not Mac Catalyst), plus a WidgetKit extension built for both
iOS/iPadOS and macOS hosts. The platform-matched extension is embedded by each
host; macOS supports only the systemSmall and systemMedium widget families.
The project explicitly lists every app source in both app targets and
`Sources/DockerAgentWidget/TeamSummary.swift` in the widget target.
`build-native.sh` validates membership before SDK builds.
The app and widget share only a small summary in the App Group
`group.dev.dockeragent.apple`; tokens, transcripts, session titles and prompt
content are not written to the shared container. The app keeps its existing
Keychain access-group identifier.

When adding or renaming app or widget sources, update the explicit PBX file
references, source phases and target membership in `project.pbxproj`.
`build-native.sh` runs `python3 Scripts/check-xcode-sources.py` first; this
verifies both app targets compile every app source exactly once and that the
widget target compiles its source exactly once.

The initial bundle identifier is the neutral, provisional
`dev.dockeragent.apple` and the display name is `Docker Agent`. Set a
distribution-owned unique bundle identifier before publishing. The iOS and
macOS app/widget targets declare the App Group
`group.dev.dockeragent.apple`; simulator builds use the existing deterministic
ad-hoc signing identity and can access the shared simulator group. Device and
distribution provisioning must enable that same App Group on both host and
extension. This project does not include a distribution team, signing identity
or provisioning profile. Do not change existing Keychain access-group names.

## Build checks

Requires an installed Xcode with iOS, iOS Simulator, and macOS SDKs. From the
`apple` directory run:

```sh
./Scripts/build-native.sh
```

The script first lists project schemes and targets, then builds the iOS
Simulator SDK with deterministic ad-hoc simulator signing, the iOS device SDK
unsigned, and the generic macOS destination unsigned. The simulator build uses
a fixed simulator-only team identifier (`FAKETEAMID`) to generate the
simulator's `application-identifier` and matching Keychain access group; this is
not a distribution identity and is not applied to device builds. The iOS builds use the explicit native target and SDK rather than a
simulator/device destination, so they compile/link the full app without
requiring a simulator runtime or connected device. The macOS build uses the
generic destination. Build products
and intermediates are isolated under `$TMPDIR/DockerAgent-XcodeDerivedData`
(override with `DERIVED_DATA_ROOT`), separate from SwiftPM's `.build`. This
compiles the simulator app but does not launch or exercise it.

To build one destination manually, for example:

```sh
xcodebuild -project DockerAgent.xcodeproj -target DockerAgent-iOS \
  -sdk iphonesimulator \
  SYMROOT="$TMPDIR/DockerAgent-XcodeDerivedData/ios-simulator/Build/Products" \
  OBJROOT="$TMPDIR/DockerAgent-XcodeDerivedData/ios-simulator/Build/Intermediates.noindex" \
  CODE_SIGNING_ALLOWED=YES CODE_SIGNING_REQUIRED=YES \
  CODE_SIGN_IDENTITY=- DEVELOPMENT_TEAM=FAKETEAMID \
  ARCHS=arm64 ONLY_ACTIVE_ARCH=YES build
```

Use `CODE_SIGNING_ALLOWED=NO` for device-SDK compile-only checks. The simulator
build embeds its simulated application identifier and Keychain group in the
app executable so Keychain calls work when the simulator launches it.

## Network and sandbox configuration

`Configuration/Info.plist` keeps App Transport Security enabled and opts only
into `NSAllowsLocalNetworking` for local-development networking. The app's
runtime `URLPolicy` is stricter: HTTPS is the default and plain HTTP is
permitted only after explicit user opt-in for `localhost`, `127.0.0.1`, or
`::1`. It never enables `NSAllowsArbitraryLoads`. The local-network usage
string explains loopback development access to the user. On macOS the app is sandboxed and receives only the outbound
`com.apple.security.network.client` entitlement needed to connect to configured
servers, plus the shared App Group entitlement for its widget. The widget has no
network client or Keychain entitlement; it reads a minimal summary snapshot and
shows an unavailable/stale state if the container has no recent data. Widget
updates are foreground-published on a bounded five-minute cadence, not a
background server connection. Distribution provisioning is required for the
shared widget on physical devices.

## Revalidate after Xcode or SDK upgrades

After a user-managed Xcode/toolchain or SDK upgrade, inspect `xcodebuild -version`
and `xcodebuild -showsdks`, then rerun `./Scripts/build-native.sh`. Review
changed SDK deprecations and ATS/sandbox behavior rather than weakening the
transport policy to make a build pass. If platform minimums change, update both
`Package.swift` and the Xcode target deployment settings together. iOS/iPadOS runtime smoke testing requires a simulator runtime/device or physical
device and should be performed separately from these unsigned generic build
checks. The macOS app has a separate local smoke mode, but no new launch-only smoke
was run for this increment. Distribution setup is intentionally outside this
project bootstrap.

## Verified build environment

Validated on 2026-10-19 with Xcode 26.6 (build 17F113), Swift 6.3.3 and
iOS/iOS Simulator/macOS 26.5 SDKs. `swift test --scratch-path
/tmp/DockerAgentCombinedFinal` passed all 137 tests in 14 suites (log:
`/tmp/DockerAgentCombinedFinal-tests.log`).
`DERIVED_DATA_ROOT=/tmp/DockerAgentCombinedFinal sh Scripts/build-native.sh`
completed with exit 0 (log: `/tmp/DockerAgentCombinedFinal-build.log`): the
simulator-signed iOS app plus embedded widget, unsigned iOS device app plus
embedded widget, and unsigned generic macOS app plus embedded widget all built
successfully. The processed simulator widget `Info.plist` was inspected and
contains `CFBundleExecutable=DockerAgentWidget`, bundle ID
`dev.dockeragent.apple.widget`, version/package metadata, and the WidgetKit
extension point. `ValidateEmbeddedBinary` succeeded. The source-membership
checker confirmed all 50 app sources compile exactly once in both native app
targets and the widget source exactly once in `DockerAgentWidget`; project and
entitlement plists passed `plutil -lint`.

The simulator SDK build validates compile, packaging and extension metadata.
Designer installed the final app+widget build over the existing simulator app
without uninstalling it or resetting profiles/Keychain, then observed the home
screen with the saved `mbp` profile and 50 sessions. The screenshot is
`/tmp/apple-design/05-home-mbp-portrait.png`. No session was opened. Automation
could not obtain assistive access, so the Agents map, actual widget gallery and
landscape behavior were not visually exercised; do not infer those flows from
the home capture. No live child/session work was attached or mutated. The focused synthetic tests
exercise team layout/zoom hit-testing and widget aggregate, deduplication and
offline projection. No live sub-agent was attached and no server work was
mutated. Distribution and physical-device App Group sharing require a profile
provisioning `group.dev.dockeragent.apple`; no distribution team/profile is
configured. No repository-wide Go build was needed because this increment
contains no Go changes.

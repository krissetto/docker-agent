# Native Apple app build

`DockerAgent.xcodeproj` is the native SwiftUI application project for iPhone,
iPad, and macOS 26. It is an iOS/iPadOS app plus a native macOS app, not
Mac Catalyst. The project explicitly lists each Swift file under
`Sources/DockerAgentApp` in both native targets and links the `AgentKit`
product from the local `Package.swift`; there are no third-party packages or
generated/binary assets. The SwiftUI `@main` entry point and
`WindowGroup` scenes are shared across platforms.

When adding or renaming Swift files under `Sources/DockerAgentApp`, update the
explicit PBX file references and the shared app group/source phase in
`project.pbxproj`. `build-native.sh` runs
`python3 Scripts/check-xcode-sources.py` first; this verifies both native targets
compile every app source exactly once and reports missing or duplicate membership
before the compiler runs.

The initial bundle identifier is the neutral, provisional `dev.dockeragent.apple`
and the display name is `Docker Agent`. Set a distribution-owned unique bundle
identifier before publishing. There is no team, signing identity, provisioning
profile, or Keychain access group configured.

## Build checks

Requires an installed Xcode with iOS, iOS Simulator, and macOS SDKs. From the
`apple` directory run:

```sh
./Scripts/build-native.sh
```

The script first lists project schemes and targets, then builds the iOS
Simulator SDK, iOS device SDK, and generic macOS destination in sequence with
`CODE_SIGNING_ALLOWED=NO`. The iOS builds use the
explicit native target and SDK rather than a simulator/device destination, so
they compile/link the full app without requiring a simulator runtime or
connected device. The macOS build uses the generic destination. Build products
and intermediates are isolated under `$TMPDIR/DockerAgent-XcodeDerivedData`
(override with `DERIVED_DATA_ROOT`), separate from SwiftPM's `.build`. This
compiles the simulator app but does not launch or exercise it.

To build one destination manually, for example:

```sh
xcodebuild -project DockerAgent.xcodeproj -target DockerAgent-iOS \
  -sdk iphonesimulator \
  SYMROOT="$TMPDIR/DockerAgent-XcodeDerivedData/ios-simulator/Build/Products" \
  OBJROOT="$TMPDIR/DockerAgent-XcodeDerivedData/ios-simulator/Build/Intermediates.noindex" \
  CODE_SIGNING_ALLOWED=NO build
```

## Network and sandbox configuration

`Configuration/Info.plist` keeps App Transport Security enabled and opts only
into `NSAllowsLocalNetworking` for local-development networking. The app's
runtime `URLPolicy` is stricter: HTTPS is the default and plain HTTP is
permitted only after explicit user opt-in for `localhost`, `127.0.0.1`, or
`::1`. It never enables `NSAllowsArbitraryLoads`. The local-network usage
string explains loopback development access to the user. On macOS the app is
sandboxed and receives only the outbound `com.apple.security.network.client`
entitlement needed to connect to configured servers. No shared Keychain group
is used.

## Revalidate after Xcode or SDK upgrades

After a user-managed Xcode/toolchain or SDK upgrade, inspect `xcodebuild -version`
and `xcodebuild -showsdks`, then rerun `./Scripts/build-native.sh`. Review
changed SDK deprecations and ATS/sandbox behavior rather than weakening the
transport policy to make a build pass. If platform minimums change, update both
`Package.swift` and the Xcode target deployment settings together. iOS/iPadOS runtime smoke testing requires a simulator runtime/device or physical
device and should be performed separately from these unsigned generic build
checks. The macOS app supports a separate launch-only local smoke mode described
below. Signing and
distribution setup are intentionally outside this project bootstrap.

## Verified build and launch environment

Validated on 2026-10-18 with Xcode 26.6 (build 17F113), Swift 6.3.3, and the
iOS, iOS Simulator, and macOS 26.5 SDKs. The final lifecycle checkpoint command was
`DERIVED_DATA_ROOT=/tmp/DockerAgentProofFinal sh apple/Scripts/build-native.sh`.
All 115 Swift tests across 14 suites also passed with
`swift test --scratch-path /tmp/docker-agent-native-final-unfiltered` (exit 0);
full test log: `/tmp/docker-agent-native-final-unfiltered.log`. `xcodebuild -list`
reported both native targets and all three unsigned build steps ended in
`BUILD SUCCEEDED`:

- `DockerAgent-iOS` with `-sdk iphonesimulator` — Simulator SDK compile/link
- `DockerAgent-iOS` with `-sdk iphoneos` — iOS device SDK compile/link
- `DockerAgent-macOS` with `-destination generic/platform=macOS`

The source-membership checker confirmed that every one of the 47 Swift files
currently under `Sources/DockerAgentApp` is included exactly once in the shared
source phase for both app targets. The simulator SDK build is not a simulator launch. `xcrun simctl list runtimes`
and `xcrun simctl list devices available` reported no installed runtimes or
devices, so simulator runtime testing was unavailable. A separate macOS
launch-only smoke check ran the built executable at
`/tmp/DockerAgentProofFinal/macos/Build/Products/Debug/DockerAgent.app/Contents/MacOS/DockerAgent`
with `--ui-smoke-test`. It remained alive for four seconds as PID 71181 with
empty captured stdout/stderr, then only that launched PID was terminated. The
smoke mode uses temporary stores, an in-memory credential store, disables
network access, and shows a local-only banner. This checks launch/liveness only;
no screenshot or visual behavior validation was performed. Successful builds
emitted non-fatal Xcode warnings: App Intents metadata extraction was skipped because the app has
no `AppIntents.framework` dependency, and the simulator SwiftPM `AgentKit`
build noted `ONLY_ACTIVE_ARCH=YES` with no active architecture available and
built all applicable architectures. No source compile errors occurred.

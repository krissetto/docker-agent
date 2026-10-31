---
title: "Contributing"
description: "Docker Agent is open source. Here's how to set up your development environment and contribute."
keywords: docker agent, ai agents, community, contributing
weight: 10
canonical: https://docs.docker.com/ai/docker-agent/community/contributing/
---

_Docker Agent is open source. Here's how to set up your development environment and contribute._

## Development Setup

### Prerequisites

- [Go 1.27](https://go.dev/dl/) or higher
- API key(s) for your chosen AI provider
- [Task](https://taskfile.dev/installation/)
- [golangci-lint](https://golangci-lint.run/docs/welcome/install/local/)

> [!NOTE]
> **Platform Support**
>
> macOS and Linux are fully supported for development. On Windows, use `task build-local` to build via Docker.

### Build from Source

```bash
# Clone and build
git clone https://github.com/docker/docker-agent.git
cd docker-agent
task build

# Set API keys
export OPENAI_API_KEY=your_key_here
export ANTHROPIC_API_KEY=your_key_here

# Run an example
./bin/docker-agent run examples/code.yaml
```

### Development Commands

| Command            | Description                                     |
| ------------------ | ----------------------------------------------- |
| `task build`       | Build the binary to `./bin/docker-agent`        |
| `task test`        | Run all tests (clears API keys for determinism) |
| `task lint`        | Run golangci-lint, custom checks, and module tidiness checks |
| `task format`      | Format code                                     |
| `task dev`         | Run lint, test, and build in parallel           |
| `task build-local` | Build for local platform via Docker             |
| `task cross`       | Cross-platform builds (all architectures)       |

## Dogfooding

Use Docker Agent to work on Docker Agent! The project includes a specialized developer agent:

```bash
cd docker-agent
docker agent run ./golang_developer.yaml
```

This agent is an expert Go developer that understands the Docker Agent codebase. Ask it questions, request fixes, or have it implement features.

## Core Concepts

- **Root Agent** — Main entry point that coordinates the system
- **Sub-Agents** — Specialized agents for specific domains
- **Tools** — External capabilities via MCP
- **Models** — AI provider configurations

## Code Style

The project uses `golangci-lint` with strict rules. As long as `task lint` passes, the code is stylistically acceptable.

Key conventions:

- Use `fmt.Errorf("context: %w", err)` for error wrapping
- Always pass `context.Context` as the first parameter
- Use `slog` for structured logging
- Use functional options pattern for constructors
- In tests: use `t.Context()`, `t.TempDir()`, `t.Setenv()`, and `t.Parallel()`

## Opening Issues

File issues on the [GitHub issue tracker](https://github.com/docker/docker-agent/issues). Please:

> [!NOTE]
> **See also**
>
> [Troubleshooting](../troubleshooting/index.md) — Common issues and debug mode. [Telemetry](../telemetry/index.md) — What data is collected and how to opt out.

- Use the included issue template
- Search for existing issues before creating new ones
- Only use issues for bugs and feature requests (not support)

## Submitting Pull Requests

1. **Fork** the repository and create a branch for your changes
2. **Write** your code following the style and testing guidelines above
3. **Test** your changes: run `task lint` and `task test`
4. **Sign off** your commits with `git commit -s` (DCO required)
5. **Open a pull request** against the `main` branch

> [!TIP]
> Use the dogfooding agent (`docker agent run ./golang_developer.yaml`) to help write and review your changes before submitting.

## Sign Your Work

All contributions require a Developer Certificate of Origin (DCO) sign-off:

```bash
# Set the identity used for the sign-off
$ git config user.name "Your Name"
$ git config user.email "your.email@example.com"

# Add a DCO sign-off to this commit
$ git commit -s -m "Your commit message"
```

`-s` adds a `Signed-off-by` trailer; it does not cryptographically sign the commit. To also sign with a GPG or SSH key, configure Git signing and use `git commit -S -s`.

## Community

Find us on [Slack](https://dockercommunity.slack.com/archives/C09DASHHRU4) for questions and discussions.

## Code of Conduct

We want to keep the Docker Agent community welcoming, inclusive, and collaborative. Key guidelines:

- **Be nice** — Be courteous, respectful, and polite. No abuse of any kind will be tolerated.
- **Encourage diversity** — Make everyone feel welcome regardless of background.
- **Keep it legal** — Share only content you own and don't break the law.
- **Stay on topic** — Post to the correct channel and avoid off-topic discussions.

The governance for this repository is handled by Docker Inc.

## Reviewer-only Gemini integration fixtures

Gemini HTTP/SSE fixtures, recorded CLI flows, and real SDK/session composition
checks are **off by default**, including fixtures that use only loopback or an
in-memory transport. Isolated adapter, configuration, policy, and metadata units
remain enabled, as do non-Gemini subtests.

Only reviewers on their own non-CI workstation may enable these fixtures with
`DOCKER_AGENT_RUN_GEMINI_INTEGRATION_TESTS=1` (exactly `1`). Any nonempty
`GITHUB_ACTIONS` value, even `false`, disables them regardless of opt-in. Do not
unset that variable to bypass the guard in GitHub Actions. No workflow or test
selection changes are needed: the guard applies to PRs (including drafts), main
and tag pushes, merge groups, manual dispatches, and the WASM test lane.

Before opting in, **deny external network egress while allowing loopback**, keep
all checked-in cassettes, and do not supply live API credentials. The E2E proxy
uses `ModeRecordOnce`: a missing cassette can attempt a real upstream request.
The OpenCode fixture also relies on a transport redirect to stay local. Opt-in
permits existing offline fixtures only, not cassette recording or paid calls.

The following commands are for reviewer execution only, not for implementers or
project CI. Run them from the repository root after checking out the PR head:

```bash
# Confirm GITHUB_ACTIONS is empty on your own workstation; never bypass it in CI.
printf 'GITHUB_ACTIONS=%s\n' "$GITHUB_ACTIONS"
export DOCKER_AGENT_RUN_GEMINI_INTEGRATION_TESTS=1

go test -count=1 ./pkg/model/provider/gemini
go test -count=1 ./pkg/model/provider -run '^TestGatewayProviderRequests$/^google$'
go test -count=1 ./pkg/runtime -run '^TestRunStream_ImageOutputGuard_RejectsBeforeDispatch$'
go test -count=1 ./pkg/tui/page/chat -run '^TestImageOutputGuard_RuntimeToTUI_RejectsBeforeDispatchAndClearsSpinner$'
go test -count=1 ./e2e -run '^TestExec_Gemini($|_)'
go test -count=1 ./e2e -run '^TestDebug_Title$/^Google_'

# Requires Node for the Go WASM test runner.
GOOS=js GOARCH=wasm go test -count=1 -tags docker_agent_wasm_cloud \
  -exec="$(go env GOROOT)/lib/wasm/go_js_wasm_exec" ./cmd/wasm \
  -run '^TestCloud(SessionsUseSessionEnvOnly|TokensReachRequests)$/.*gemini'
GOOS=js GOARCH=wasm go test -count=1 \
  -exec="$(go env GOROOT)/lib/wasm/go_js_wasm_exec" ./cmd/wasm \
  -run '^TestGoogleFactory(RejectsVertexWithoutCloudTag|KeepsGeminiAPIWithoutCloudTag)$'
```

Expected: selected fixtures run rather than skip and their existing assertions
pass. They cover streaming keepalives/tool replay, request wire shape, auth and
transport wrapping, embeddings, image-output guards, runtime/TUI error propagation,
CLI tools/title/sampling, and WASM session/token flows. These are existing
regressions, not live-backend entitlement or new model acceptance tests. Record
head SHA, OS/Go/Node versions, command, pass/fail/skip and sanitized evidence.
A skip is not a pass, and pure unit success is not integration evidence.

For the preparation change, no Gemini integration fixture was executed by the
implementer, even to verify the skip path. Validation is limited to pure gate
units, source placement review, compile-only checks, static analysis and builds.
Future fixture additions must be guarded before server, proxy, transport, SDK
flow or browser-session setup, with pure cases and non-Gemini rows kept separate.

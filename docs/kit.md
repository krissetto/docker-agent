# Async subagents Kit v3

`kit/` contains only the descriptor, companion Dockerfile and ignore file,
launcher, [seven-agent team](../kit/hackerspace.yaml) and initial user settings.
Offline regression tests live in `tests/kit/`.

## Build

From the repository root, with Docker Buildx and an OCI-export-capable builder
(for example the `docker-container` driver):

```sh
task kit                                      # kagent:local-v3
task kit -- namespace/kagent:trial
KIT_PLATFORM=linux/amd64,linux/arm64 task kit -- namespace/kagent:trial
```

This **build-only** task exports a unique archive under ignored
`dist/kit/v3.*/kit.oci.tar`: no load, prompt or push. Pass at most one image
reference after `--`; it is not evaluated as shell input. `KIT_PLATFORM`
overrides `DOCKER_DEFAULT_PLATFORM`; the default is `linux/amd64`.
Task is optional; use direct Buildx for other options:

```sh
docker buildx build . -f kit/async-agent.yaml --platform linux/amd64 \
  --tag namespace/kagent:trial --output type=oci,dest=/tmp/async-agent.oci.tar
```

Keep the **repository root as build context**, since the recipe needs `go.mod`,
`cmd/` and `pkg/`. The descriptor's `dockerfile: ./async-agent.dockerfile` is
relative to the descriptor; Dockerfile `COPY` paths are relative to the root.
The inspected SBX source loader cannot use this nested layout: passing the root
misses the descriptor; passing `kit/` loses Go sources. Use Buildx, not source
symlinks or `sbx run kit/`.

The [ignore file](../kit/async-agent.dockerfile.dockerignore) denies inputs by
default, allowing only compilation inputs, embedded assets and selected runtime
assets. Tests, fixtures, `.git`, dotenv files and databases stay out. Matching
untracked Go sources are included; review source for hardcoded secrets.
The binary is statically compiled and reports `dev` / `local-source`, not a
clean-commit claim. Task 3.53.1 is checksum-pinned for Linux amd64/arm64.
`--build-arg kitVersion=0.1.1` changes Kit packaging metadata, not the app version.

### Publication

An OCI archive is not automatically imported into SBX. Docker `--load` loses Kit
metadata: do not publish that image with `docker push` as a Kit. Use a repository
ending in **`kagent`**, not `docker-agent`: v3 registration uses the final repository
component and otherwise collides with built-in `docker-agent`.

Publishing is a separate maintainer action. The pinned frontend emits Kit
annotations on platform manifests; consumers also need descriptor, schema-version
and capabilities annotations on the top-level index. Plain `buildx --push` is not
proof of a usable Kit. Use matching frontend/spec metadata with expanded build
arguments, preserve platforms and provenance, and verify the remote index and
referenced blobs before sharing an immutable reference. The local task contains
no annotation-repair or publisher framework.

The descriptor pins the frontend matching spec
`v3.0.0-m.6.0.20260929191907-34df175ec869`; use a compatible Kit v3 consumer.
Do not strip derived package metadata for an older consumer or silently widen
optional permissions.

## Run

Use a **verified release containing these sources**. Set `KIT_REF` to its immutable
registry reference, for example `namespace/kagent@sha256:<verified-digest>`; the
placeholder is not a published artifact. You need compatible SBX, pull access if
private, and approved credentials in **SBX's host credential store**. Model calls
incur charges. Use a fresh workspace and sandbox name for changed builds:

```sh
workspace="$HOME/async-agent-trial-1"
mkdir "$workspace" &&
sbx run --name async-agent-trial-1 "$KIT_REF" "$workspace" -- \
  --model openai/ACCESSIBLE_MODEL
```

A global `--model` overrides every agent; omit it to use the bundled models.
`ACCESSIBLE_MODEL` means a model your account can use. Append `--exec --dry-run`
for initialization without a model turn; SBX may still require credentials.
Intentionally reattach with the same name, model and team arguments:
`sbx run --name async-agent-trial-1 -- --model openai/ACCESSIBLE_MODEL`.

The launcher uses workspace `hackerspace.yaml`, falling back to the bundled team.
An explicit team must be the first argument after SBX's `--`:

```sh
sbx run "$KIT_REF" /path/project -- --team './my team.yaml' \
  --model openai/ACCESSIBLE_MODEL
```

`--team=PATH` also works. Paths resolve inside the sandbox, not arbitrary host
files. Missing, unreadable or non-file paths fail. Remaining arguments pass
unchanged, including `--exec`, `--session ID`, `--session=-1` and prompts after `--`.

## Credentials and network

| Service | Requirement | Guest sentinel | Exact injection hosts |
| --- | --- | --- | --- |
| OpenAI | Required | `OPENAI_API_KEY` | `api.openai.com` |
| Anthropic | Required | `ANTHROPIC_API_KEY` | `api.anthropic.com` |
| Google | Optional | `GOOGLE_API_KEY` | `generativelanguage.googleapis.com` |
| GitHub | Optional | `GH_TOKEN` | `api.github.com`, `github.com` |

All credentials are runtime-only and proxy-managed: real keys stay on the host,
not in guest environment, YAML, build arguments or workspaces. Both required
bindings remain required with a single-provider model override. Store secrets
using the **host** SBX CLI, preferably sandbox-scoped:

```sh
NAME=async-agent-trial-2
sbx secret set openai --sandbox "$NAME"
sbx secret set anthropic --sandbox "$NAME"
# Optional:
sbx secret set github --sandbox "$NAME"
sbx create --name "$NAME" "$KIT_REF" /path/to/workspace
sbx run --name "$NAME"
```

Masked `secret set` works before creation. An existing host GitHub CLI can supply
its token through `sbx secret set github --sandbox "$NAME" --command 'gh auth token --hostname github.com'`;
SBX captures the output on the host. Do not print tokens or expose host CLI config
to sandbox writes. Secret storage and approval of exact domain bindings are
separate steps; noninteractive creation is not automatic approval. Successful
startup does not prove authentication: the inspected SBX warns on missing
required bindings but still withholds unapproved credentials.

The base includes `gh` and `git`. Use credential-free HTTPS Git remotes; the proxy
adapts GitHub authentication. Leave `GH_HOST` unset or `github.com`, not
`api.github.com`. Never run guest login, token export, debug credential output or
credential-helper setup. Public API reachability is not authentication proof;
check intended restricted reads with a least-privilege host token.

SSH remotes, GitHub Enterprise/CDN/redirect hosts, Copilot, OAuth, Azure/custom
endpoints, Bedrock, Vertex and local DMR/Ollama are not granted. Review additional
routes separately; never wildcard credential injection.

## Optional integrations and defaults

Optional means a host may omit an integration, not automatic approval or a
promise of enforcement. All agents share workspace files; sessions do not
provide filesystem isolation.

- **Context:** `ASYNC_AGENT_KIT.md` names host/composed-kit guidance beside the
  workspace; this kit ships no redundant context prose. All seven agents discover
  it and project `AGENTS.md`. Missing optional content is harmless.
- **Skills:** readonly `/home/agent/.agents/skills` is enabled only for Shelly,
  Engineer and Designer. Other roles retain restricted tools. Review skills as
  untrusted executable input; readonly does not prevent execution. A custom team
  replaces, not inherits, context/skills loaders.
- **Identity/signing:** Git attribution is separate from optional runtime-only
  SSH-agent signing (`unrestricted: false`, `sign: [git]`, no authentication).
  Configure an approved public signing key separately; never import private keys
  or host Git config.
- **State:** optional `/home/agent/.cagent` covers sessions/data, not settings or
  custom database paths. The image precreates it agent-owned `0700`; existing
  volumes need reviewed ownership. Runtime controls retention and capacity.
  Keep SQLite WAL/FULL durability on sandbox-local Linux storage, not shared or
  network filesystems. Reusing old custom-branch databases is not guaranteed.
- **Detached lifetime:** background processes may survive client disconnect; this
  does not keep the TUI attached, restart the app or resume idle subagents.
- **Sessions:** prompt `--exec -- PROMPT`, resume `--session ID`, continue
  `--session=-1`. `/opt/async-agent/docker-agent sessions list --quiet` lists root
  IDs newest first, without model startup, using the same data/config as resume.
- **Diagnostics:** off by default; `--kit-arg diagnostics=on` permits local markers
  under `/home/agent/.local/state/async-agent-kit/probe`. Hooks run as agent without
  network or credentials; install appends per invocation, startup replaces `ready`.
- **Settings:** [user-config.yaml](../kit/user-config.yaml) hides the banner,
  enables **YOLO (automatic tool approval)** and restores tabs within saved state.
  Config directories are agent-owned `0700`, settings `0600`. Ordinary Settings
  can change them; the launcher never resets them. Mounted/custom config wins.

The launcher forces auto-update off, disables telemetry and uses the SBX workspace
as working directory. Keep transcripts private and ensure UID/GID 1000 can write
the workspace. Do not combine this workload with built-in Docker Agent.

## Offline checks

```sh
go test -count=1 ./tests/kit
go vet ./tests/kit
```

Tests cover launcher behavior, team/models, provider grants, optional integration
contracts, synthetic host context/skills discovery, diagnostics, settings and
build-input isolation. They do not prove live SBX permission enforcement, daemon
hook timing, proxy authentication or model entitlement.

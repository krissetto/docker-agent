# Async subagents Kit v3

The kit packages Docker Agent, its launcher and the seven-agent
[hackerspace team](../kit/hackerspace.yaml). `kit/` contains only the descriptor,
companion Dockerfile and ignore file, launcher, team and initial user settings.
Offline regression tests live in `tests/kit/`.

## Build from this checkout

From the repository root, with Docker Buildx and an OCI-export-capable builder
(for example the `docker-container` driver):

```sh
task kit                                      # kagent:local-v3
task kit -- namespace/kagent:trial
KIT_PLATFORM=linux/amd64,linux/arm64 task kit -- namespace/kagent:trial
```

This is a **build-only** task: it exports a unique archive under ignored
`dist/kit/v3.*/kit.oci.tar`, without loading, prompting or publishing. Task is
optional; the equivalent direct build is:

```sh
docker buildx build . -f kit/async-agent.yaml --platform linux/amd64 \
  --tag namespace/kagent:trial --output type=oci,dest=/tmp/async-agent.oci.tar
```

`KIT_PLATFORM` overrides `DOCKER_DEFAULT_PLATFORM`; the default is `linux/amd64`.
Pass at most one image reference after `task kit --`; it is passed as one literal
Buildx tag, not evaluated as shell input. Use direct Buildx for other options.

The **repository root must remain the build context**: the Dockerfile needs
`go.mod`, `cmd/` and `pkg/`. The descriptor's `dockerfile:
./async-agent.dockerfile` is relative to the descriptor; Dockerfile `COPY` paths
are relative to the root context.

The inspected SBX local-source loader **does not support this nested source
layout**. Passing the root misses the nested descriptor; passing `kit/` loses the
Go sources and registers `kit`, not `kagent`. A descriptor file is not a local
kit reference. Do not substitute symlinks: source hashing may miss changes.
Build with Buildx and launch a published immutable reference instead.

### Publication boundary

An OCI archive is not automatically imported into SBX. A Docker `--load` image
loses Kit metadata and must not be published with `docker push` as a Kit.
Choose a repository ending in **`kagent`**, not `docker-agent`: Kit v3 registration
uses the repository's last component and otherwise collides with the built-in
`docker-agent`. Namespace, tag, display name and entrypoint cannot fix that.

Publication is an explicit, separate maintainer action. The pinned frontend emits
Kit annotations on platform manifests; consumers also need the descriptor,
schema-version and capabilities annotations on the top-level OCI index. A plain
`buildx --push` is **not sufficient evidence** of a usable Kit. Before publishing,
use matching frontend/spec metadata with expanded build arguments, preserve
every platform and provenance manifest, and verify the remote index and referenced
blobs. The local task deliberately does not contain
an annotation-repair or publishing framework. Share only a verified immutable
registry reference.

### Build inputs and checks

The [companion ignore file](../kit/async-agent.dockerfile.dockerignore) denies
inputs by default, allowing compilation inputs, embedded assets and the selected
runtime assets. Tests, fixtures, `.git`, dotenv files, databases and unrelated
artifacts stay out. Matching untracked Go sources are included; review code for
hardcoded secrets before building. No provider credentials belong in build
arguments, image environment, team YAML or workspace files.

The binary is statically compiled with `no_audio` and verified with `xx-verify
--static`. Its version reports `dev` / `local-source`, not a clean-commit claim.
Frontend and runtime base are digest-pinned; builder dependencies are not a fully
hermetic lock. Task **3.53.1** is bundled for Linux amd64 and arm64 using pinned
archive checksums; it needs no runtime installation or Go toolchain.
`--build-arg kitVersion=0.1.1` changes the Kit packaging version, provide and image
label together, not the Docker Agent release version.

Offline checks (no publication, credentials or model calls):

```sh
go test ./tests/kit
go vet ./tests/kit
```

Tests cover the actual launcher, team restrictions/models, provider policy,
optional integrations, context/skills discovery, settings and build allowlist.
They are not a general Kit-spec conformance suite and do not prove SBX runtime
permission enforcement, proxy authentication or model entitlement.

The current frontend is `docker/sandbox-kit:3` pinned to
`sha256:a8659fa579de7e8d8dc5b5712feba5efdabceb953279106e9c770d2cd8ca52f1`,
source revision `34df175ec8696ab0a1ad76116b358595552b6794` (reported dirty), matching
spec `v3.0.0-m.6.0.20260929191907-34df175ec869`. New builds need a compatible Kit v3
consumer; the historical artifact below used m.5. Optional integrations may be
omitted, never silently widened. Older m.3 consumers also reject some derived
Debian package provides; update the consumer rather than strip package metadata.

## Run a published kit

Consumers need a compatible SBX installation, registry pull access if private,
and approved credentials in **SBX's host credential store**, not this checkout,
Go, Python or local compilation. Model conversations incur charges. Choose a
fresh dedicated workspace and sandbox name for each changed build.

### Historical publication status

The previously published and remotely verified **linux/amd64, m.5** artifact is:

```sh
KIT_REF='christopherpetito053/kagent@sha256:1c2ec35cc46b1886e2774e34249a764c8ae703f4e8e30828fef6ea3de4b28442'
workspace="$HOME/async-agent-trial-1"
mkdir "$workspace" &&
sbx run --name async-agent-trial-1 "$KIT_REF" "$workspace" -- \
  --model openai/ACCESSIBLE_MODEL
```

It was published as `christopherpetito053/kagent:subagents-reborn` by copying the
original OCI artifact without an application rebuild. Its index and referenced
blobs were verified; the official m.5 artifact TCK passed 15 checks. These are
**historical claims**, not validation of current sources or mutable tag contents.
That artifact is OpenAI-only, has six agents, and predates the leading `--team`
option and current optional integrations/settings. It does not enable YOLO.
`christopherpetito234/kagent` remains unpublished and unverified.

Do not use the legacy
`christopherpetito053/docker-agent@sha256:1c2ec35cc46b1886e2774e34249a764c8ae703f4e8e30828fef6ea3de4b28442`:
it collides with built-in registration. Those original tags returned `not found`
during the historical registry recheck.

`ACCESSIBLE_MODEL` means a model available to your account. A global `--model`
overrides every agent, including Designer in current builds. Append
`--exec --dry-run` for initialization without a model turn; SBX can still require
host credential setup. No live SBX/proxy authentication or model entitlement is
established by image builds or network-disabled dry runs.

Intentional reattachment uses the same name and model arguments:

```sh
sbx run --name async-agent-trial-1 -- --model openai/ACCESSIBLE_MODEL
```

### Team selection (current source builds)

Without a leading `--team`, the launcher selects workspace `hackerspace.yaml`,
falling back to the bundled team. An explicit team must be the first option after
SBX's `--`, before Docker Agent arguments:

```sh
# KIT_REF must identify a verified release containing this launcher.
sbx run "$KIT_REF" /path/project -- --team './my team.yaml' \
  --model openai/ACCESSIBLE_MODEL
```

`--team=PATH` also works. Paths resolve inside the sandbox; this does not mount
arbitrary host files. Missing, unreadable or non-file paths fail, without silently
falling back. Remaining arguments are forwarded unchanged, including `--exec`,
`--session ID`, `--session=-1` and prompt text after `--`. Reuse the same team
selection when intentionally reattaching or resuming.

The bundled team has seven roles. Shelly (`root`), Planner and Reviewer use
OpenAI `gpt-6.1-sol` with high thinking, Engineer medium and Greppy low. Director
uses `gpt-6-astra` high; Designer uses Anthropic `claude-opus-5-5` adaptive/high.
Engineer is the default implementer, Designer a targeted UX/frontend specialist.
Director's only base tool is filesystem `read_file`; Greppy and Planner are
read-only discovery/analysis roles. Independent review is requested only when
the user asks. Unused Anthropic/DMR model aliases remain in the supplied team;
local LAN models require separately reviewed network access.

## Credentials and network policy (current source builds)

| Service | Requirement | Guest sentinel | Exact injection hosts | Header |
| --- | --- | --- | --- | --- |
| OpenAI | Required | `OPENAI_API_KEY` | `api.openai.com` | `Authorization: Bearer %s` |
| Anthropic | Required | `ANTHROPIC_API_KEY` | `api.anthropic.com` | `x-api-key: %s` |
| Google | Optional | `GOOGLE_API_KEY` | `generativelanguage.googleapis.com` | `x-goog-api-key: %s` |
| GitHub | Optional | `GH_TOKEN` | `api.github.com`, `github.com` | `Authorization: Bearer %s` |

All are runtime-only and proxy-managed. Real credentials stay on the host;
guest variables are sentinels. OpenAI and Anthropic declarations remain required
even with a global single-provider override. Credential presence does not select
a model. Google uses `GOOGLE_API_KEY`, not a duplicate `GEMINI_API_KEY` binding.
These grants do not cover OAuth, Copilot, Azure/custom endpoints, Bedrock,
Vertex, local DMR/Ollama or credential refresh.

Use the host SBX CLI with a consistent host user/config directory. Prefer
sandbox-scoped secrets; these are operator instructions, not validation commands:

```sh
NAME=async-agent-trial-2
sbx secret set openai --sandbox "$NAME"
sbx secret set anthropic --sandbox "$NAME"
# Optional:
sbx secret set github --sandbox "$NAME"
sbx create --name "$NAME" "$KIT_REF" /path/to/workspace
sbx run --name "$NAME"
```

`secret set` uses masked input and works before sandbox creation. Without
`--sandbox`, secrets are global. An existing host GitHub CLI can supply a token
through `sbx secret set github --sandbox "$NAME" --command 'gh auth token --hostname github.com'`;
SBX captures that output on the host. Never print the token or place host CLI
configuration in sandbox-writable mounts.

Storing a secret and approving its domain binding are separate steps. Review
only the exact hosts above. Host approvals live in
`$XDG_CONFIG_HOME/sbx/credentials.yaml` (normally `~/.config/sbx/credentials.yaml`);
noninteractive creation is not automatic approval. The inspected SBX warns and
allows launch with missing required bindings, contrary to the spec's required
resolution behavior; it still withholds unapproved credentials. Successful
startup is not authentication proof.

The pinned base includes `gh` and `git`. Use credential-free HTTPS remotes such
as `https://github.com/OWNER/REPO.git`; the proxy adapts GitHub auth for HTTPS Git.
Leave `GH_HOST` unset or `github.com`, not `api.github.com`. Do not run guest login,
`gh auth token`, `--show-token`, `GH_DEBUG=api` or credential-helper setup.
Permissions come from the host token, not the sentinel. Public `gh api rate_limit`
is not authentication proof; test an intended restricted read instead.
`gh auth status` can misreport GitHub App tokens behind a proxy.

SSH remotes, Enterprise, Copilot, GHCR, uploads, raw content, LFS and GitHub/CDN
redirect hosts are not granted. Review extra routes separately; never wildcard
credential injection or send tokens to redirected hosts.

## Optional integrations and runtime boundaries

These require a new build, not the historical reference. `optional: true` allows
omission, not automatic approval or a promise of enforcement by every host.

- **Context:** the optional declaration names `ASYNC_AGENT_KIT.md` for guidance
  contributed by a host or composed kit. This kit ships no redundant context
  prose. All bundled agents discover that profile beside the workspace and
  project `AGENTS.md`; missing optional content is harmless.
- **Skills:** readonly `/home/agent/.agents/skills` is the normal discovery path.
  Only Shelly, Engineer and Designer enable skills. Other roles retain restricted
  tools. Skills may contain shell commands: readonly is not execution isolation;
  review them as untrusted input. A custom team replaces, not inherits, these
  context/skills settings. An explicit `DOCKER_AGENT_KIT_DIR` changes discovery.
- **Identity/signing:** Git identity supplies attribution only. Optional SSH-agent
  access is runtime-only, `unrestricted: false`, `sign: [git]`, with no SSH
  authentication grant. Git still needs an approved public signing key and SSH
  signing configuration. Do not import host Git config or private keys.
- **State:** optional `/home/agent/.cagent` stores sessions/data, not settings.
  The image precreates it agent-owned `0700`, also usable without a volume.
  SBX's inspected block-volume implementation does not enforce descriptor mode;
  existing volumes need operator-reviewed ownership. Retention, capacity and
  deletion/reuse behavior belong to the runtime. Custom data/database paths are
  not covered. Keep SQLite WAL/FULL durability on sandbox-local Linux storage,
  not host-shared or network filesystems.
- **Detached lifetime:** long-running support permits background processes after
  disconnect; it does not keep a TUI attached, restart the app, resume idle
  subagents or guarantee autonomous continuation.
- **Sessions:** prompt is `--exec -- PROMPT`, resume `--session ID`, continue
  `--session=-1`. Listing runs the complete command
  `/opt/async-agent/docker-agent sessions list --quiet`: full root-session IDs,
  newest first, no model startup. It uses the same data/config context as resume;
  restored children are idle, not restarted computations.
- **Diagnostics:** off by default. `--kit-arg diagnostics=on` permits local
  lifecycle markers under `/home/agent/.local/state/async-agent-kit/probe`, never
  the workspace. Install appends per invocation; startup atomically replaces
  `ready`. Hooks run as agent without network or credentials. Local shell tests
  do not establish daemon hook timing.
- **Settings:** new builds seed [user-config.yaml](../kit/user-config.yaml), hiding
  the startup banner, enabling **YOLO (automatic tool approval)** and restoring
  tabs within the same saved state. Agent-owned config directories are `0700`,
  settings `0600`. Ordinary Settings can change these; the launcher never resets
  them. Mounted/custom config takes precedence. Non-kit defaults, existing
  sandboxes and historical images are unchanged.

All agents share workspace files; async sessions are not filesystem isolation.
The launcher forces auto-update off, disables telemetry and uses the SBX
workspace as working directory. Keep transcripts private. Use fresh sandbox
names/workspaces for changed builds; old custom-branch database migrations are
not a compatibility promise. The base provides UID/GID 1000, bash, Git, CA store
and inherited Docker-in-Docker overhead. Ensure that user can write the trial
workspace. Do not combine this workload with the built-in Docker Agent kit.

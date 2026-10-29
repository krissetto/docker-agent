# Async subagents Kit v3

`kit/` contains only the descriptor, companion Dockerfile and ignore file,
launcher, [seven-agent team](../kit/hackerspace.yaml) and initial user settings.
Offline regression tests live in `tests/kit/`.

## Build and push

From the repository root, with Docker Buildx, a multi-platform-capable builder
(for example the `docker-container` driver), and registry push access:

```sh
task kit                       # docker.io/christopherpetito053/kagent
task kit -- namespace/kagent    # override the repository, not a tag
```

This task **builds and pushes** `linux/amd64` and `linux/arm64` in one Buildx
invocation, tagging both `:latest` and `:$GIT_COMMIT`. `GIT_COMMIT` defaults to the
full current Git commit SHA and must be 40 lowercase hexadecimal characters.
Pass only a repository after `--`, without a tag or digest; it is quoted, not
evaluated as shell input. The task builds the working tree, including allowed
untracked sources: a SHA tag alone does not establish a clean-commit build.
A successful task publishes the multiarch Kit; no separate metadata promotion or
verification command is required.

For a build-only local archive instead, invoke Buildx directly:

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

The descriptor pins the latest inspected development frontend for specification
commit [`210ccc3aa393fc67295cc4058ba3d65c6f98be92`](https://github.com/docker/sandbox-kit-spec/commit/210ccc3aa393fc67295cc4058ba3d65c6f98be92):
`docker/sandbox-kit:210ccc3@sha256:8944c950eaaef7e1f33bd4c672e9607c8deb33eedd49d62d8ac58b2d4045de99`.
Anonymous Docker Registry metadata verified this immutable multi-platform
manifest index on 2026-10-18; amd64 and arm64 are present. Both the old and new
frontends use Kit schema v3. The frontend tag is a commit identifier, not a Kit
schema-version tag. The commit is dated 2026-10-16. Build provenance was not
separately inspected. Main commits publish short-SHA frontend tags rather than
moving floating `:3`.

The Kit keeps host-agent Git signing optional and bounded inline rather than
composing the official [`git-signing@1.0.0` mixin](https://github.com/docker/sandbox-kit-spec/blob/210ccc3aa393fc67295cc4058ba3d65c6f98be92/examples/git-signing/git-signing.yaml)
by default. That mixin requires `ssh-agent@1` and enables missing Git signing
defaults, changing this Kit's default authority and behavior. The published [`sbx/git-ssh-sign-kit:latest`](https://hub.docker.com/r/sbx/git-ssh-sign-kit)
artifact is schema v2 (manifest `sha256:003e6d1d9e4381f058cc1a3336cf1c8d539af02ddb9ebb0edb0405631a2f4ece`),
not a v3 mixin. SBX runtime
support for `ssh-agent@1` was added at source commit
[`466c6d4`](https://github.com/docker/sandboxes/commit/466c6d48107179a10cd65bdf3c8d2dc6cf06748b);
the installed `v0.21.0-6505` (`aed956f`) predates it. Use an updated SBX consumer
with v3 `ssh-agent@1` support; the old source-load path is not evidence that the
installed CLI rejects published v3 descriptors. No release number is inferred
from either source commit.

This frontend emits the expanded descriptor, schema-version and capabilities
annotations on both platform manifests and the top-level index. `task kit`
builds and publishes that index directly, preserving Buildx's provenance defaults;
it does not rewrite the index or run the Kit. Use a compatible Kit v3 consumer.
The optional `agent-interactive-sessions@1` declaration requires a consumer that
implements the new terminal session surface; older consumers may omit it. The
separate `agent-sessions@1` surface is headless, including resume and continue.
No lifecycle interactive tail is necessary: the default launch already opens
the TUI. Both session surfaces list the same managed history.

The descriptor retains `network-policy@1` and the context's omitted `directory`,
which preserves workspace-sibling discovery. It does not request host sharing or
alternate context placement. Do not strip derived package metadata for an older
consumer or silently widen optional permissions.

For a deliberate creation-time opt-in, the official
[`git-signing`](https://github.com/docker/sandbox-kit-spec/blob/210ccc3aa393fc67295cc4058ba3d65c6f98be92/examples/git-signing/git-signing.yaml)
and [`github-ssh`](https://github.com/docker/sandbox-kit-spec/blob/210ccc3aa393fc67295cc4058ba3d65c6f98be92/examples/github-ssh/github-ssh.yaml)
source mixins can be added to this published workload using a consumer that
supports repeated `--kit` source refs. The references pin source to the inspected
spec commit. To keep both signing and GitHub SSH access for the last example,
include both mixins as shown; use the published
`$KIT_REF` workload, not `sbx run kit/` (this repository's Dockerfile needs the
repository root as build context):

```sh
SPEC_SHA=210ccc3aa393fc67295cc4058ba3d65c6f98be92
sbx create --name async-agent-signing \
  --kit "git+https://github.com/docker/sandbox-kit-spec.git#ref=$SPEC_SHA&dir=examples/git-signing" \
  "$KIT_REF" "$workspace"

# Broader opt-in: also permit GitHub's SSH login, on port 22, with pinned host keys.
sbx create --name async-agent-github-ssh \
  --kit "git+https://github.com/docker/sandbox-kit-spec.git#ref=$SPEC_SHA&dir=examples/git-signing" \
  --kit "git+https://github.com/docker/sandbox-kit-spec.git#ref=$SPEC_SHA&dir=examples/github-ssh" \
  "$KIT_REF" "$workspace"
```

The `git-signing` mixin makes Git signing required and fills missing Git
signing defaults. `github-ssh` also includes signing defaults and additionally
requests authentication only as `git@github.com`, permits `github.com:22`, and
installs GitHub host keys; it is not general SSH access. Using both repeats the
signing bounds, which the v3 merger unions and marks required. Host approval and
SSH-agent availability remain required. These `--kit` source refs are optional
additions: neither is in this Kit's published workload unless explicitly added.
The CLI resolves/builds each pinned Git source through its Kit frontend/BuildKit
path, distinct from the published workload's root-context Buildx build. A v3
`kind: set` can pin only published registry refs (with digests when published),
not Git source refs; publish a set first if the composition needs a reusable
immutable registry reference. See [v3 set reference rules](https://github.com/docker/sandbox-kit-spec/blob/210ccc3aa393fc67295cc4058ba3d65c6f98be92/docs/spec/SPEC-v3.md#34-kit-set).

## Run

Use a **published release containing these sources**. Set `KIT_REF` to its immutable
registry reference, for example `namespace/kagent@sha256:<published-digest>`; the
placeholder is not a published artifact. You need compatible SBX, pull access if
private, and approved credentials in **SBX's host credential store**. Model calls
incur charges. Use a fresh workspace and sandbox name for changed builds:

```sh
workspace="$HOME/async-agent-trial-1"
mkdir "$workspace" &&
sbx create --name async-agent-trial-1 \
  --kit-arg model=openai/ACCESSIBLE_MODEL "$KIT_REF" "$workspace"
sbx run --name async-agent-trial-1
```

### Creation-bound server configuration

The Kit deliberately differs from standalone `docker-agent run`: workspace,
model override and private state belong to **sandbox creation**. The team normally
comes from the creation-bound `team` argument, but Kit connections can select a
workspace YAML with the first argument after `--`, for example:

```sh
sbx run ourkit:latest -- ./myagent.yaml
sbx run ourkit:latest --exec --dry-run -- "./teams/my agent.yaml"
```

SBX forwards arguments after `--` to the Kit entrypoint. A leading `.yaml` or
`.yml` argument is treated as the team path (relative to SBX's requested guest
working directory, which must be inside the workspace); remaining arguments
retain their usual prompt/flag meaning. The path must resolve to a readable
regular file inside the mounted workspace. Symlinks are resolved and rejected
if they escape the workspace. Host files are not uploaded automatically. Missing,
unreadable, invalid or out-of-workspace YAML fails before a session is created;
it never falls back to the bundled team. Each successful registration snapshots
that file and its configuration dependencies in private managed state. Existing
running sessions keep their current runtime owner; subsequent cold resumes and
new sessions use the latest registered revision for that same workspace-relative
path. Different paths remain separate session sources. Creation-time `team`
selection remains the default when no team path is passed.

Startup uses SBX's `WORKSPACE_DIR` when supplied, otherwise the startup process
working directory. `/workspace` is the image default, not a
forced mount location. The connection preserves SBX's requested working
directory; `--working-dir` can select a session directory inside that workspace
without changing the lifecycle-owned API's workspace identity.

Create arguments are:

| Kit argument | Default | Meaning |
| --- | --- | --- |
| `team` | `auto` | Workspace `hackerspace.yaml`, otherwise bundled team; or a readable guest file path relative to the startup workspace or absolute. |
| `model` | empty | Team-wide model override, e.g. `openai/ACCESSIBLE_MODEL`; comma-separated targeted overrides follow the CLI's normal model syntax. Empty keeps team models. |
| `teamEdit` | `off` | Creation-bound trusted team-edit grant: `off`, `restricted`, or `full`. This is authority, not team YAML; only grant it when the sandbox harness permits it. |
| `stateDir` | `/home/agent/.cagent/managed-api` | Absolute private guest state base outside the mounted workspace. |
| `diagnostics` | `off` | Local install/boot markers; no provider calls. |

For a consumer supporting SBX's `--kit-arg` creation interface, configure the
team at sandbox creation (this also selects the API's default team):

```sh
sbx create --name async-agent-trial-1 \
  --kit-arg model=openai/ACCESSIBLE_MODEL \
  --kit-arg 'team=./my team.yaml' \
  --kit-arg teamEdit=restricted "$KIT_REF" /path/project
sbx run --name async-agent-trial-1
# Initialization without a model turn:
sbx run --name async-agent-trial-1 -- --exec --dry-run
# Reattach to retained history:
sbx run --name async-agent-trial-1 -- --session=-1
```

Use only a model your account can access. Both required provider credential
bindings remain required with a single-provider override. `team=auto` and no
`model` argument retain the bundled/workspace defaults. Creation paths are guest
paths, not arbitrary host files. Missing, unreadable and non-file team paths fail.

Connection-time `--team`, `--model`, and non-default `--team-edit` are rejected;
recreate with `team`, `model`, and `teamEdit` respectively. A client cannot grant
or elevate team-edit access on an existing API. The Kit exposes only the global
mode (no per-agent allowlist); use `off` unless your trusted harness specifically
allows a source-wide restricted/full grant. `--managed-api-state-dir` cannot change the creation-bound
base: recreate with `stateDir`. `--working-dir` selects the session directory,
not a different API; the server rejects paths outside the creation workspace.
Server-changing flags (`--env-from-file`, `--flavor`, `--models-gateway`,
`--code-mode-tools`, runtime hook flags, `--mcp-oauth-redirect-uri`, explicit
remote auth/workdir, `--listen`, `--session-workingdir-root`) are unsupported on
Kit connections, with no corresponding Kit create argument. Use reviewed
creation-time team/settings configuration or standalone CLI for those options.
Managed fake/record, worktree and local database modes remain unsupported.
`--exec`, `--dry-run`, `--session ID`, `--session=-1`, agent selection, prompts
following `--`, and client `--safety` / `--yolo` remain connection options.
Standalone managed/remote CLI flexibility is unchanged.

The Kit starts with the Docker Next - Dark theme (`docker-next-dark`). The theme
sets colors only; for an optional host-side dedicated font, see
[Custom terminal fonts](terminal-fonts.md). Choose another theme in the TUI or
pass `--theme NAME` when connecting to override it.

### Lifecycle-owned API and recovery

Required `lifecycle@1` runs `start.sh` as agent on every boot with
`background: true`. The script execs one **foreground** managed API, not a
launcher that detaches another daemon. It freezes creation-bound team/models
using the existing managed manifest, private token, canonical workspace hash,
owner lock and database. No new mailbox or runtime owner is introduced.
`launch.sh` waits up to 30 seconds for authenticated identity/readiness and
attaches a TUI or headless client; session listing also attaches only. Neither
starts/restarts an API or opens a local database. Missing hooks/dead servers
fail clearly; restart the sandbox instead of relying on a connection fallback.

**Client exit detaches, not cancels accepted server work.** Explicit cancellation
remains necessary to stop work. Required `long-running@1` prevents sandbox
session-based auto-stop; it is not process supervision. After an API failure,
explicit sandbox stop/start runs startup again. Durable session state and the
stable token/endpoint survive on retained sandbox disk, but in-memory execution,
interrupted tools and side effects are not automatically replayed. Deleting the
sandbox deletes its local state.

Frozen team/model/startup changes fail rather than silently reconfiguring or
killing existing work, including after restart. Retain original team inputs or
create a separate sandbox/state base deliberately. Context/skills, if configured
by the team, remain live discovery inputs, not frozen file contents.

The spec's [creation-time environment expansion](https://github.com/docker/sandbox-kit-spec/blob/210ccc3aa393fc67295cc4058ba3d65c6f98be92/docs/spec/SPEC-v3.md#61-final-container-environment)
persists team/model/state argv for restart. Hook inheritance is deny-by-default:
the descriptor lists credential sentinels, proxy/CA settings, approved SSH socket,
config/tool paths and sandbox detection names explicitly; shell baseline is
provided by the consumer. Real keys must never enter argv, snapshots or guest
files. Arbitrary custom environment names/providers are not implicitly admitted;
review and extend the descriptor before using them. No additional credential
services, domains, permissions or host mounts are granted.

**Live consumer support remains unverified.** The actual consumer must implement
required lifecycle/background launch, workspace mapping, final-env expansion and
runtime credential sentinel/proxy availability before the foreground process
starts. An allowlist alone cannot make absent startup credentials available.
Offline checks do not establish DAP/cloud/SBX conformance or model entitlement.

### API access and troubleshooting

The Kit's managed listener binds guest `0.0.0.0:9182` so SBX port publication
can reach it. Port `9182` is fixed for Kit startup; on the next API ownership
startup it overrides a previously saved ephemeral endpoint. A collision fails
rather than falling back to another port or stopping an existing listener.
Non-Kit managed APIs continue to select/reuse an ephemeral port by default. Managed discovery and local Kit clients continue using
`http://127.0.0.1:9182`. The server requires its private bearer token on all API
requests, including when reached through a published port. The token is
kept in the guest's private managed state; broad binding does not make it a host
credential.

This exposes the API to any peer that can reach a guest interface, not just the
sandbox itself. Authenticated clients have API-wide tool-execution authority;
they are trusted peers, not isolated users or sessions. Host-port publication
is a separate operator choice; prefer a host-loopback binding (for example host
`127.0.0.1:8888` mapped to guest port `9182`) unless wider exposure has been
deliberately reviewed. For access beyond the local host,
use TLS or a trusted tunnel, and explicitly approve any browser origins; this Kit
does not configure those or grant browser CORS. Do not expose the private token.
The Kit currently always broad-binds; this is intentional for sandbox port
forwarding. Standalone and non-Kit managed APIs retain their loopback default.

This source change affects only newly built Kit images and future startup. It has
not been activated in an existing sandbox; activation requires rebuilding the Kit
and restarting/recreating the sandbox so its lifecycle startup runs the new
command.

Default managed state is under
`/home/agent/.cagent/managed-api/<canonical-workspace-sha256>/`. Creation-time
`stateDir=BASE` changes the base, not the workspace hash suffix.
The directory is agent-owned `0700`; private `0600` files include `token`,
`manifest.json`, `server.json`, `server.log` and `session.db`. The manifest contains
frozen configuration; logs and the database can contain sensitive session data.
Keep them out of the mounted workspace, transcripts and bug reports. Never print
or share the token. The daemon alone owns its database and locking; do not start
a second local client against that database or remove locks/state to bypass an
incompatibility error.

For readiness failures, use the reported stage. If `manifest.json` is missing,
startup did not commit managed state; inspect SBX's lifecycle startup log at
`/var/log/sbx-kit-startup.log` for an early config/load error. `server.log` is
created only after startup configuration and snapshot parsing succeed, so it may
not exist for those errors. When the manifest exists, inspect the reported probe
error and `server.log` locally, confirm state ownership/modes and free disk space,
and check that the original workspace and startup inputs are still available. Stale server metadata is recovered under locks; a mismatched
identity or occupied endpoint fails without killing another process. A
settings/custom database path is not a substitute for the managed state base.
Session listing from inside the guest uses:

```sh
/opt/async-agent/docker-agent sessions list --managed-api --managed-api-attach \
  --managed-api-state-dir "$ASYNC_AGENT_KIT_STATE_DIR" --quiet
```

Both descriptor session surfaces use the creation-bound state base. API discovery
uses runtime `WORKSPACE_DIR`, falling back to the listing process cwd; it does not
require a workspace value during descriptor resolution. Kit listing includes
sessions in workspace subdirectories, uses the committed server source
and creates no sessions; a dead API yields an error rather than restarting it
or opening a second local database.

## Credentials and network

The descriptor requests capabilities when the sandbox is created; the host decides
whether to approve them and is responsible for enforcing its policy. Optional
capabilities may be omitted by a host. Changing a Kit request requires recreating
the sandbox with a reviewed Kit descriptor.

| Capability | Status | Enables | Does not enable |
| --- | --- | --- | --- |
| Network policy and OpenAI/Anthropic credentials | Required | Proxy-managed model API requests to the exact hosts below | Guest-visible provider keys or other egress |
| Google/GitHub credentials | Optional (requested) | Proxy-managed requests to the exact hosts below | Other providers, GitHub Enterprise, or wildcard injection |
| Headless and interactive agent sessions | Optional (requested) | Resume, continue, prompt, and list through the managed API | Starting a second local server or database |
| Context and skills | Optional (requested) | Workspace context discovery and readonly skills mount | Filesystem isolation or trusted skill execution |
| Git identity | Optional (requested) | Git author attribution | Authentication or signing configuration |
| Git commit signing via host SSH agent | Optional (requested) | Restricted Git object signing | SSH login, SSH remotes, or exporting private keys |
| Lifecycle and long-running process | Required | Start the managed API and keep the sandbox alive while it runs | Process supervision or recovery of interrupted work |
| Team edit | Creation argument (`off`) | Trusted team YAML edits when creation-time authority is granted | Per-agent grants or elevation after creation |
| ACP protocol integration | Not requested | — | The app does not provide ACP support |
| Volume mounts and GitHub SSH authentication | Not requested | — | No host filesystem mounts or SSH remote authentication |

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
startup does not prove provider authentication. Actual startup sentinel delivery
and proxy enforcement are unverified; conforming consumers must reject missing
required bindings and withhold unapproved credentials.

The base includes `gh` and `git`. Use credential-free HTTPS Git remotes; the proxy
adapts GitHub authentication. Leave `GH_HOST` unset or `github.com`, not
`api.github.com`. Never run guest login, token export, debug credential output or
credential-helper setup. Public API reachability is not authentication proof;
check intended restricted reads with a least-privilege host token. The optional
SSH agent request is for Git commit signing only; see the separate signing opt-in
below.

This Kit does not request SSH authentication or SSH remotes. GitHub SSH access
(`authenticate: [git@github.com]`), GitHub Enterprise/CDN/redirect hosts, Copilot,
OAuth, Azure/custom endpoints, Bedrock, Vertex and local DMR/Ollama are not
requested. The separate official `github-ssh` source kit demonstrates GitHub
SSH authentication and network access; that is distinct from this Kit’s Git
commit-signing request. Review additional routes separately; never wildcard
credential injection.

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
- **Git identity:** The optional Git identity capability supplies commit author
  attribution; it does not authenticate, authorize signing, or configure a
  signing key.
- **Git commit signing via host SSH agent:** Optional, runtime-only, and
  restricted to Git signing (`ssh-agent@1`, `unrestricted: false`, `sign: [git]`).
  This is not SSH login or SSH remote access. The host keeps the private key; the
  guest requests signatures through the approved signing socket. The capability
  is requested by the Kit, but the host must approve it and enforce the policy.
  To opt in, load the key into the host agent and approve the capability. In the
  target repository, set an author principal and signing locally (replace the
  path with the approved public-key file available in the guest):

  ```sh
  git config --local user.email 'your-signing-principal@example.com'
  git config --local gpg.format ssh
  git config --local user.signingkey /path/to/approved-signing-key.pub
  git config --local commit.gpgsign true
  ```

  SSH signature verification requires an allowed-signers file. Create one from
  the public key and configure verification locally before checking a signed
  commit:

  ```sh
  gitdir=$(git rev-parse --absolute-git-dir)
  principal=$(git config --local --get user.email)
  printf '%s %s\n' "$principal" "$(cat /path/to/approved-signing-key.pub)" > "$gitdir/allowed_signers"
  git config --local gpg.ssh.allowedSignersFile "$gitdir/allowed_signers"
  git log --show-signature -1
  ```

  Signing is never enabled automatically by this Kit. If Git signing is enabled
  but the approved agent is absent, the commit should fail rather than silently
  become unsigned. Never import private keys, copy host `~/.ssh` or Git
  configuration, or load private key files into the guest.
- **State:** managed sessions/data use
  `/home/agent/.cagent/managed-api/<canonical-workspace-sha256>/` on the sandbox's
  own disk, not a host mount, workspace directory or separate volume. The image
  precreates `/home/agent/.cagent` agent-owned `0700`; settings are separate.
  State persists while that sandbox's disk is retained, not after its deletion or
  in a fresh sandbox. Runtime controls retention and capacity. There is no live
  migration or automatic repair of existing mounts; an older sandbox with a state
  mount may need to be replaced with a fresh sandbox. Keep SQLite WAL/FULL
  durability on sandbox-local Linux storage, not shared or network filesystems.
  Reusing old custom-branch databases is not guaranteed.
- **Detached lifetime:** the lifecycle-owned foreground API survives TUI exit;
  required long-running support prevents session-based auto-stop, but does not
  supervise/restart crashed processes or preserve interrupted execution.
- **Sessions:** headless prompt `--exec -- PROMPT`, resume `--exec --session ID`,
  continue `--exec --session=-1`; interactive prompt `-- PROMPT`, resume
  `--session ID`, continue `--session=-1`, and a fresh session with no extra argv.
  Interactive invocations require a terminal. No dedicated startup picker is
  declared. The attach-only list command above
  lists root IDs newest first from the same server-owned history used by resume,
  without creating sessions or opening a second local database.
- **Diagnostics:** off by default; `--kit-arg diagnostics=on` permits local markers
  under `/home/agent/.local/state/async-agent-kit/probe`. Diagnostic hooks run as
  agent without network or credentials; install appends per invocation, startup
  replaces `ready`. This marker means the diagnostic hook
  ran, not API readiness; authenticated API probing is separate.
- **Settings:** [user-config.yaml](../kit/user-config.yaml) hides the banner,
  enables **YOLO (automatic tool approval)** and restores tabs within saved state.
  Config directories are agent-owned `0700`, settings `0600`. Ordinary Settings
  can change them; the launcher never resets them. Mounted/custom config wins.

The launcher forces auto-update off, disables telemetry and uses the SBX workspace
as working directory. Keep transcripts private and ensure UID/GID 1000 can write
the workspace. Do not combine this workload with built-in Docker Agent.

## Offline checks

```sh
sh -n kit/launch.sh kit/start.sh
go test -count=1 ./tests/kit
go vet ./tests/kit
```

Tests cover launcher behavior, team/models, provider grants, optional integration
contracts, synthetic host context/skills discovery, diagnostics, settings and
build-input isolation. They do not prove live SBX permission enforcement, daemon
hook timing, proxy authentication or model entitlement.

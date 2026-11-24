# Async subagents Kit v3

`kit/` contains the descriptor, companion Dockerfile and ignore file,
launchers, [agent team](../kit/hackerspace.yaml), initial user settings,
and separately selected source mixins under `kit/opt-ins/`.
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
commit [`d841decb96d9d676a3813ad46cd05583591cdcb3`](https://github.com/docker/sandbox-kit-spec/commit/d841decb96d9d676a3813ad46cd05583591cdcb3):
`docker/sandbox-kit:d841dec@sha256:f5116e23e492215c8dcb1fcba8f43bb49f465adc7251d128e4c64a3af2eeb4f3`.
Anonymous Docker Registry metadata verified this immutable multi-platform
manifest index on 2026-11-20; amd64 and arm64 are present. Both the old and new
frontends use Kit schema v3. The frontend tag is a commit identifier, not a Kit
schema-version tag. The commit is dated 2026-11-20. Build provenance was not
separately inspected. Main commits publish short-SHA frontend tags rather than
moving floating `:3` for every commit. Latest released milestone
`v3.0.0-m.13` (`cc033f80d58bb9516db1c26b71a3ce8184100fd0`, a
prerelease) and floating `:3` both currently resolve to
`sha256:922fd778877714b2353de978c4993b81fe2e1908793630205eedbd7229ca0a1f`.
Main is eleven commits ahead, with no schema/library capability changes beyond
m.13; its spec text clarifies volume mount order and workspace shadowing.

The Kit keeps host-agent Git signing optional and bounded inline rather than
composing the official [`git-signing@1.0.0` mixin](https://github.com/docker/sandbox-kit-spec/blob/d841decb96d9d676a3813ad46cd05583591cdcb3/examples/git-signing/git-signing.yaml)
by default. That mixin requires `ssh-agent@1` and enables missing Git signing
defaults, changing this Kit's default authority and behavior. The published [`sbx/git-ssh-sign-kit:latest`](https://hub.docker.com/r/sbx/git-ssh-sign-kit)
artifact is schema v2 (manifest `sha256:003e6d1d9e4381f058cc1a3336cf1c8d539af02ddb9ebb0edb0405631a2f4ece`),
not a v3 mixin. SBX runtime
support for `ssh-agent@1` was added at source commit
[`466c6d4`](https://github.com/docker/sandboxes/commit/466c6d48107179a10cd65bdf3c8d2dc6cf06748b);
the previously inspected `v0.21.0-6505` (`aed956f`) predates it. That is a
historical observation, not a current installed-version check; SBX is unavailable
in this validation environment. Use a consumer with v3 `ssh-agent@1` support.
The old source-load path is not evidence that a CLI rejects published v3
descriptors. No release number is inferred from either source commit.

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
which preserves workspace-sibling discovery. The optional `agent-context@2`
profile declares only `filename: ASYNC_AGENT_KIT.md`; workload context remains
inline without an explicit `delivery` field. A packaged `hooks.d` turn-start
drop-in loads `ASYNC_AGENT_KIT.md` for bundled and custom teams without rewriting
team YAML or user settings. Its independent context group preserves user prompt-file
contexts and hooks. A custom config directory replaces these packaged defaults;
install the drop-in there explicitly if desired. Do not select both context
versions: optional `@1` and `@2` declarations are not alternatives and conflict.
It does not request host sharing or
alternate context placement. Do not strip derived package metadata for an older
consumer or silently widen optional permissions.

For a deliberate creation-time opt-in, the official
[`git-signing`](https://github.com/docker/sandbox-kit-spec/blob/d841decb96d9d676a3813ad46cd05583591cdcb3/examples/git-signing/git-signing.yaml)
and [`github-ssh`](https://github.com/docker/sandbox-kit-spec/blob/d841decb96d9d676a3813ad46cd05583591cdcb3/examples/github-ssh/github-ssh.yaml)
source mixins can be added to this published workload using a consumer that
supports repeated `--kit` source refs **and has capability-selection policy
accepting only `agent-context@2`**. These official sources declare both optional
context versions; a consumer accepting both cannot compose them with this
workload. Optionality does not select an alternative. If your consumer cannot
omit `agent-context@1` before merging, do not use these official source examples;
the local default-off mixins below do not have that conflict. The references
pin source to the inspected spec commit. To keep both signing and GitHub SSH access for the last example,
include both mixins as shown; use the published
`$KIT_REF` workload, not `sbx run kit/` (this repository's Dockerfile needs the
repository root as build context):

```sh
SPEC_SHA=d841decb96d9d676a3813ad46cd05583591cdcb3
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
immutable registry reference. See [v3 set reference rules](https://github.com/docker/sandbox-kit-spec/blob/d841decb96d9d676a3813ad46cd05583591cdcb3/docs/spec/SPEC-v3.md#34-kit-set).

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

To diagnose a slow reopen, pass `--debug --log-file /tmp/kit-reopen.log` as
Kit connection arguments. For a large-session reattach, add `--session ID` and
use a separate log file. Record the wall time from requesting the tab to the
first usable screen; the guest log only measures after the guest process starts.
Extract the content-free timing records from the guest log before sharing:

```sh
grep -E 'msg="(Managed API|Session transport|Session presentation) timing"' /tmp/kit-reopen.log
```

These records separate authenticated readiness, optional source registration,
view requests, stream baseline delivery and transcript presentation. Transport
`headers_elapsed` is time to response headers; `response_bytes` counts decoded
HTTP body bytes read, not TCP traffic. A standalone reproduction can also pass
`--debug --log-file /tmp/kit-server-timing.log` to `serve api --managed-api`
and extract `msg="Session stream timing"` for server-side baseline preparation
and writes. Do not share full debug logs, which can contain unrelated details.

**Client exit detaches, not cancels accepted server work.** Explicit cancellation
remains necessary to stop work. Required `long-running@1` prevents sandbox
session-based auto-stop; it is not process supervision. Optional
`auto-restart@1` requests recovery only after a host or runtime
restart: a previously running sandbox restarts and its startup hooks run again.
An explicit operator stop remains stopped. Unsupported consumers may skip this
optional capability; manual sandbox stop/start remains available. This is not
supervision of an in-guest API crash. After an API failure, explicit sandbox
stop/start runs startup again. Durable session state and the
stable token/endpoint survive on retained sandbox disk, but in-memory execution,
interrupted tools and side effects are not automatically replayed. Deleting the
sandbox deletes its writable-layer state unless the separate retained-state
opt-in below was selected. Retained volumes have a different lifetime.

Frozen team/model/startup changes fail rather than silently reconfiguring or
killing existing work, including after restart. Retain original team inputs or
create a separate sandbox/state base deliberately. Context/skills, if configured
by the team, remain live discovery inputs, not frozen file contents.

The spec's [creation-time environment expansion](https://github.com/docker/sandbox-kit-spec/blob/d841decb96d9d676a3813ad46cd05583591cdcb3/docs/spec/SPEC-v3.md#61-final-container-environment)
persists team/model/state argv for restart. Hook inheritance is deny-by-default:
the descriptor lists `IS_SANDBOX` (with legacy `SANDBOX_VM_ID`), credential sentinels, proxy/CA settings, approved SSH socket,
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
| Host/runtime recovery | Optional (requested) | Restart a previously running sandbox after host/runtime recovery | Restarting explicitly stopped sandboxes, in-guest crash supervision, or replaying interrupted work |
| Lifecycle and long-running process | Required | Start the managed API and keep the sandbox alive while it runs | Process supervision or recovery of interrupted work |
| Team edit | Creation argument (`off`) | Trusted team YAML edits when creation-time authority is granted | Per-agent grants or elevation after creation |
| ACP protocol integration | Optional (requested) | Stdio ACP v1 bridge with workspace/team-scoped list, load and resume of shared managed history | A second runtime/database, client-owned filesystem tools, or modal elicitation |
| Host browser, Docker, registry auth, host services, retained volumes | Default off; separate source mixins | Only the explicitly selected integration below | No implicit host socket, host filesystem mount, or wildcard registry/service grant |
| GitHub SSH authentication | Not requested by default | Separate official mixin | No unrestricted SSH authentication |

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
  workspace; this kit ships no redundant context prose. The packaged turn-start
  hook loads it for every agent, including custom teams;
  project `AGENTS.md` discovery remains an independent team/user choice. Missing
  optional content is harmless.
- **Skills:** readonly `/home/agent/.agents/skills` is enabled only for Shelly,
  Engineer and Designer. Other roles retain restricted tools. Review skills as
  untrusted executable input; readonly does not prevent execution. A custom team
  retains its own skills and project prompt-file loaders; the packaged global
  context hook is separate.
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
  Without the retained-state opt-in, state persists while that sandbox's disk
  is retained, not after deletion or in a fresh sandbox. Runtime controls
  retention and capacity. There is no live
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
  File edits and Git review use split diffs. All regular assistant replies stay
  visible outside collapsed Work. Sending a new prompt scrolls it smoothly to
  the top of the chat. The sidebar starts on the left, extends beside
  the input dock, and shows Plans when there are plans to display.
  Config directories are agent-owned `0700`, settings `0600`. Ordinary Settings
  can change them; the launcher never resets them. Mounted/custom config wins.

The launcher forces auto-update off, disables telemetry and uses the SBX workspace
as working directory. Keep transcripts private and ensure UID/GID 1000 can write
the workspace. Do not combine this workload with built-in Docker Agent.

### Creation-time integrations

These directories are **independent v3 source mixins**, not workloads or image
build contexts. They contain no context-version declarations, so they compose
with this workload's `agent-context@2`. The workload preserves its Docker intent
as an **optional** `dockerd@1` request with `autoStart: true`: an approved,
supported request starts a guest-private engine; denial or an unsupported host
leaves the standalone workload usable without Docker. **`optional: true` is
not an off switch**: it is a request that a host may omit. Browser opening,
retained state, registry authentication and host-port access remain default-off.
The workload alone requests none of `open-url`, `docker-auth`, `host-port`,
`volume`, `host-mount` or standalone `privileged`. Installing tooling is not an
access grant. The mixins remain absent until explicitly included using `--kit`;
once selected their requests are required and fail closed if unsupported or
unapproved. The Docker mixin makes the base's optional request required for
workloads that cannot operate without Docker.

The commands below are examples only: they create sandboxes and request access,
so run them only after reviewing the effective permission surface. They do not
publish or build the workload. Use an already published immutable `$KIT_REF`
containing these changes, a real `$workspace`, and a v3 consumer supporting local
source mixins and `--kit-arg`. Run from this checkout's root. SBX may resolve the
metadata-only mixin sources through its frontend/BuildKit path. The workload
still uses the published reference, **not** `sbx run kit/`.

```sh
# Standalone default: optional Docker; denial/unsupported still permits use.
sbx create --name async-agent-plain "$KIT_REF" "$workspace"

# Host browser navigation (runtime host, not necessarily your CLI machine).
sbx create --name async-agent-browser \
  --kit ./kit/opt-ins/open-url "$KIT_REF" "$workspace"

# Require the private in-sandbox Docker daemon; never the host Docker socket.
sbx create --name async-agent-docker \
  --kit ./kit/opt-ins/dockerd "$KIT_REF" "$workspace"

# Retain private settings and managed history under this owner scope/name.
sbx create --name async-agent-retained \
  --kit ./kit/opt-ins/retained-state "$KIT_REF" "$workspace"

# One exact operator-chosen registry; require dockerd when Docker is essential.
# ghcr.io is only an example, not an implicit default or wildcard.
sbx create --name async-agent-registry \
  --kit ./kit/opt-ins/dockerd --kit ./kit/opt-ins/docker-auth \
  --kit-arg registry=ghcr.io "$KIT_REF" "$workspace"

# One host loopback service: host IPv4 TCP 5432 -> guest 127.0.0.1:15432.
sbx create --name async-agent-database \
  --kit ./kit/opt-ins/host-port \
  --kit-arg hostPort=5432 --kit-arg guestPort=15432 \
  --kit-arg transport=tcp --kit-arg family=ipv4 "$KIT_REF" "$workspace"
```

Repeated `--kit` options **compose**: include every desired mixin in the same
creation command; a later independent create example does not retain a previous
example's choices. Names above deliberately differ. None is an activation or
migration procedure for an existing sandbox. Local sources are working-tree
inputs, not immutable pins; review them before use. To distribute an immutable
composition, publish reviewed mixins first and pin their registry digests in a
v3 set. This change publishes nothing.

- **Browser:** `open-url@1` supplies `xdg-open` and `$BROWSER`; startup explicitly
  admits `BROWSER` to the API's environment. Only HTTP/HTTPS navigation is
  provided, without per-URL confirmation. The browser is on the runtime host;
  its cookies, authenticated sessions, redirects and local-network access are
  outside sandbox network policy. There is no DOM/browser control, host file
  opening or automatic OAuth callback forwarding. Without the mixin, print URLs
  for manual opening instead. Only grant this to trusted workloads.
- **Docker:** the pinned `shell-docker` base carried
  `com.docker.sandboxes.start-docker=true`, an implicit legacy request for
  elevation, an engine and storage. The Dockerfile explicitly overrides it to
  `false`; old inspected runtime code checks for the exact string `true`.
  Dockerfile inheritance cannot remove that key. Consumers that elevate merely
  because the label exists are incompatible with this consent-gated packaging:
  the legacy path must not bypass denial of the optional capability.
  `dockerd@1` expresses the base's intent optionally; the selected Docker mixin
  instead requires it, with `requires: [deb/docker-ce]` so a composition lacking
  packaged engine tooling refuses. Under pinned spec §7.1 and §9.5 and the
  [`dockerd@1` Composition contract](https://github.com/docker/sandbox-kit-spec/blob/d841decb96d9d676a3813ad46cd05583591cdcb3/docs/spec/capabilities/com.docker.sandbox/dockerd%401.md#composition),
  these contributions collapse to **one**
  required request, not two daemons: presence unions, required wins over optional,
  and `autoStart` is true if any contribution says true or omits it. When granted,
  it requests elevation and private `/var/lib/docker`, starts the engine before
  startup, and makes its guest socket usable by the image user.
  It does not share `/var/run/docker.sock` from the host. Engine storage survives
  stop/start and in-place replacement, but is deleted with the sandbox; it does
  not share retained-volume lifetime. Never add a volume at or under
  `/var/lib/docker`. No redundant default `privileged@1` fallback is requested.
- **Registry authentication:** `registry` is required, without a default.
  `docker-auth@1` admits only that exact lowercase dotted DNS registry, with
  strict effective-config validation of DNS length and IP exclusion as well as
  the argument pattern. Schemes, ports, paths and wildcards are invalid.
  The mixin adds only `<registry>:443` runtime egress. Cross-host authentication,
  image-layer CDN and redirect routes are **not** guessed or broadly allowed;
  for registries needing them, review an additional exact network-policy mixin
  naming only documented endpoints. Docker Hub commonly needs distinct
  authentication/layer hosts; selecting `docker.io` alone does not promise a
  successful pull. A conforming runtime keeps real login secrets and issued
  tokens host-side and provides opaque run-scoped placeholders. It must fail
  closed for unsupported exchanges. Prefer read-only repository-scoped host
  bindings. Do not mount host Docker config, copy helpers or run guest login.
  This grant does not install Docker, start a daemon, or publish images.
- **Host service:** ports are required decimal integers 1–65535 with no leading
  zeros; transport is `tcp` or `udp`, family `ipv4`, `ipv6`, or `dual` (defaults
  `tcp`/`ipv4`). `dual` asserts both host families serve the **same** service.
  The host endpoint is runtime-host loopback, never an arbitrary address; the
  guest endpoint is always IPv4 `127.0.0.1`. Avoid guest API port **9182** and
  every other occupied/published port. Runtime collision validation remains
  required. This is raw access to that service, not application authentication
  or filtering. It needs no additional network-policy entry. Clients must
  bypass HTTP proxies for loopback, e.g. `curl --noproxy '*' ...`. Nothing is
  activated by merely shipping the descriptor.

#### Private retained settings and history

`retained-state` requests agent-writable, initially `0700` block volumes at
`/home/agent/.config/cagent` and `/home/agent/.cagent`, not host sharing or
workspace mounts. A fresh mount hides the image defaults without copying them;
its agent install hook seeds **only missing** settings and the independent
context hook from immutable `/opt/async-agent/default-*.yaml` files. Existing
files are never overwritten; a custom config directory remains authoritative.
A runtime unable to provide private, agent-writable storage must refuse or fail
the install hook, not silently fall back. The hook does not chown/chmod existing
retained data or request additional privilege.

Settings and managed history then survive sandbox deletion and recreation under
the **same owner scope and sandbox name**, when these paths are selected again.
A new name starts empty. Omitting the mixin leaves retained volumes unmounted,
not deleted; selecting it later may reveal that name's previous private data to
the new composition after approval. Equivalent retained path/size/mode/tmpfs
requests are required; incompatible changes must refuse without data loss.
Inspect and explicitly remove retained storage only with your runtime's
operator storage commands; deletion of the sandbox alone is **not** erasure.
No migration, copying of old writable-layer state, volume removal, cleanup or
live activation is provided or performed by this change. Backups and host/disk
loss durability remain runtime/operator responsibilities.

The default `stateDir=/home/agent/.cagent/managed-api` lies inside the retained
state volume. An override remains retained only when it lies beneath
`/home/agent/.cagent` (outside the workspace); an arbitrary other base is not
magically covered. Workspace canonical hashes still partition managed state:
a different canonical workspace path does not automatically discover old history.
Managed tokens, endpoint/owner metadata, frozen source snapshots, logs and
SQLite history are retained too. They remain private and sensitive: replacing
this workload under the same name can expose those data to the newly approved
composition. Stable auth does not imply active execution survives recreation,
nor that stale/frozen configuration incompatibilities can be bypassed. Never
print the token, reset its state to fix an error, or attach a second DB owner.

### Managed-backed ACP

The optional `acp@1` declaration names the existing `async-agent-kit` provide,
uses `/opt/async-agent/acp.sh` as a complete stdio command without a TTY, and
speaks protocol major 1. It attaches to the same authenticated lifecycle-owned
API using creation-bound team/workspace/state, rather than starting standalone
`serve acp` or opening another SQLite/runtime owner. Unsupported consumers may
omit the declaration. The ordinary TUI/headless entrypoint and stdio launcher
behavior are unchanged.

ACP sessions share the API's durable history with Kit headless and TUI sessions,
not a separate ACP database. An attached client can list, load and resume
root sessions within the configured workspace, including its authorized
subdirectories, whose persisted team/source matches the connection, including
sessions created in the TUI. Paths are canonically checked; symlinks escaping
the workspace, child sessions and other teams are not included. This is not an
API-wide history browser. Loading replays supported saved conversation content;
resuming attaches without replay.
Attachment and replay do not execute a turn, so busy sessions' saved history can
still be inspected. Closing the ACP client or session detaches its connection
rather than deleting managed history or shutting down the lifecycle-owned API.

Sending a new ACP prompt requires an idle session. The server atomically rejects
busy sessions and pending interactions rather than queuing the prompt, cancelling
another client's turn or taking over its pending approvals. ACP cancellation and
permission responses apply only to its own accepted turn.

Every ACP-started turn requires editor approval for tool calls, even when the
shared session was created by the Kit TUI with `YOLO: true`. This is a transient,
turn-scoped constraint: the same session ID and history can continue through ACP
without changing the stored policy or how later TUI turns are approved. Existing
denials remain enforced; automatic approval hooks and saved grants cannot bypass
this constraint. Lasting approval responses are limited to the current request,
not stored as permission for future turns. The approval constraint remains with
the accepted input through recovery or retry until it finishes; a missing editor
does not silently approve tools. New ACP sessions also start with strict approval
policy.

Execution and tools remain server-owned. The managed API reads and writes the
sandbox filesystem using its configured tools; ACP client filesystem or terminal
capabilities do not replace those tools, upload host files, or expose unsaved
editor buffers. This differs from standalone `serve acp`, which can mediate
supported file operations through the client's advertised filesystem methods.
Clients that require all filesystem access to run through the editor, or assume
host paths identify guest files, are not compatible with this managed mode.
Provide needed editor context as embedded text or images, and make files needed
by server tools available in the sandbox workspace.

Managed ACP does not accept client-supplied MCP servers, additional directories
or resource-link prompt
blocks; send embedded text instead of client file references. Modal elicitation
during an ACP-started turn remains unsupported. A dead API, source mismatch or
unsupported interaction fails rather than starting a fallback local agent. No
second local runtime or database owner is created.

### Upgrade boundaries

The prior frontend source pin was `210ccc3aa393fc67295cc4058ba3d65c6f98be92`.
The new schema remains v3. It adds `agent-context@2`, `host-port@1`,
`dockerd@1`, `docker-auth@1`, `open-url@1`, and `auto-restart@1`; existing
contracts add port address families, retained-volume lifetime, authoritative
sandbox environment, strict NUL checks, credential rendering rules, and
`args.buildSecret` for BuildKit secret mounts. Named build contexts are also
supported. Existing headless/interactive session argv contracts are unchanged.

Context, optional host/runtime recovery and managed-backed ACP are declared
in the workload, along with optional Docker elevation and a guest-private engine.
The required-Docker mixin enforces that existing intent when Docker is essential.
Browser opening, host services, registry authentication and retained private
volumes are separate default-off mixins, never implicitly selected by this
workload. Host filesystem sharing remains unrequested.
`buildSecret` is available for future private build inputs, not needed by this
recipe. These contracts are draft-stage changes within schema v3: use matching
readers and runtime handlers. Upgrading a frontend does not establish runtime
support or add capabilities to existing published Kits/sandboxes.

## Offline checks

```sh
sh -n kit/launch.sh kit/start.sh kit/acp.sh
go test -count=1 ./tests/kit
go vet ./tests/kit
```

Tests cover launcher behavior, team/models, provider grants, optional integration
contracts, packaged global context hooks for bundled/custom teams, synthetic
host context/skills discovery, diagnostics, settings and
build-input isolation. They do not prove live SBX permission enforcement, daemon
hook timing, proxy authentication or model entitlement.

### Authoritative schema check

The updated descriptor was also validated against all 29 JSON Schema files at
`d841decb96d9d676a3813ad46cd05583591cdcb3`, fetched as data from
`https://raw.githubusercontent.com/docker/sandbox-kit-spec/<commit>/schema/`.
To repeat with a separately obtained checkout at that exact commit and locally
installed Node `ajv` and `js-yaml` packages, run from this repository root:

```sh
SPEC_DIR=/path/to/sandbox-kit-spec node tests/kit/validate-schema.cjs
```

The check validates all six authored descriptors, structurally expands example
create arguments, rejects missing/invalid registry and host-service inputs, and
schema-checks every one of the 32 selection subsets after bounded reconciliation
of the overlapping Docker, network-policy and lifecycle declarations under pinned
§9.5. It asserts exactly one Docker request, optional without the Docker mixin
and required with it; it also covers Docker auto-start union and required-wins
cases. This is not a general capability merger and does not prove the consumer's
implementation, package resolution or live permission gates.
Declaration-only `kind: mixin` sources need no Dockerfile or filesystem recipe
under the pinned spec (§3.5); each directory has exactly one descriptor. Their
required inputs must still be supplied at create.

No remote code is executed by that check; after obtaining the schema data it can
run offline. Some Ajv installations warn about the optional `iconUrl` URI format;
this descriptor has no `iconUrl`. Static schema validation is not a frontend
build, artifact conformance verdict, or live runtime test. After a local OCI
build, `kit-tck validate --layout <unpacked-oci-layout> <tag>` checks the built
artifact without a registry. Runtime conformance is separate and must not be
inferred from either check.

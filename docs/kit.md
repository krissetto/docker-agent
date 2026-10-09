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

The descriptor pins the published development frontend for specification commit
[`a1d110de252bcc3cb2fa6d47fe37137a1a328b97`](https://github.com/docker/sandbox-kit-spec/commit/a1d110de252bcc3cb2fa6d47fe37137a1a328b97):
`docker/sandbox-kit:a1d110d@sha256:7b5be06981e0a3b808b8043ad3f2d5edc5df3645ce628c9bae1bca0fc4334557`.
GitHub remote HEAD was rechecked on 2026-10-09; the commit is dated 2026-10-08.
Anonymous registry HTTP metadata confirmed the immutable Linux amd64/arm64 index
digest. Build provenance was not separately inspected. Main commits publish
short-SHA frontend tags rather than moving floating `:3`.

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
team, model override and private state belong to **sandbox creation**, not each
connection. A conforming `sbx@1` consumer maps the host workspace to the image's
explicit `/workspace` WorkingDir. There is no alternate workspace argument.
Startup explicitly changes to that path; it never assumes hook cwd.

Create arguments are:

| Kit argument | Default | Meaning |
| --- | --- | --- |
| `team` | `auto` | Workspace `hackerspace.yaml`, otherwise bundled team; or a readable guest file path relative to `/workspace` or absolute. |
| `model` | empty | Team-wide model override, e.g. `openai/ACCESSIBLE_MODEL`; comma-separated targeted overrides follow the CLI's normal model syntax. Empty keeps team models. |
| `stateDir` | `/home/agent/.cagent/managed-api` | Absolute private guest state base outside the mounted workspace. |
| `diagnostics` | `off` | Local install/boot markers; no provider calls. |

For a consumer supporting SBX's `--kit-arg` creation interface:

```sh
sbx create --name async-agent-trial-1 \
  --kit-arg model=openai/ACCESSIBLE_MODEL \
  --kit-arg 'team=./my team.yaml' "$KIT_REF" /path/project
sbx run --name async-agent-trial-1
# Initialization without a model turn:
sbx run --name async-agent-trial-1 -- --exec --dry-run
# Reattach to retained history:
sbx run --name async-agent-trial-1 -- --session=-1
```

Use only a model your account can access. Both required provider credential
bindings remain required with a single-provider override. `team=auto` and no
`model` argument retain the bundled/workspace defaults. Paths are guest paths,
not arbitrary host files. Missing, unreadable and non-file team paths fail.

Connection-time `--team` and `--model` are rejected: recreate with `team` and
`model` respectively. `--managed-api-state-dir` cannot change the creation-bound
base: recreate with `stateDir`. `--working-dir` is rejected: select the host
workspace when creating the sandbox; its guest location is `/workspace`.
Server-changing flags (`--env-from-file`, `--flavor`, `--models-gateway`,
`--code-mode-tools`, runtime hook flags, `--mcp-oauth-redirect-uri`, explicit
remote auth/workdir, `--listen`, `--session-workingdir-root`) are unsupported on
Kit connections, with no corresponding Kit create argument. Use reviewed
creation-time team/settings configuration or standalone CLI for those options.
Managed fake/record, worktree and local database modes remain unsupported.
`--exec`, `--dry-run`, `--session ID`, `--session=-1`, agent selection, prompts
following `--`, and client `--safety` / `--yolo` remain connection options.
Standalone managed/remote CLI flexibility is unchanged.

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

The spec's [creation-time environment expansion](https://github.com/docker/sandbox-kit-spec/blob/a1d110de252bcc3cb2fa6d47fe37137a1a328b97/docs/spec/SPEC-v3.md#61-final-container-environment)
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

The managed listener binds guest **loopback only**, uses a private bearer token,
and grants authenticated clients API-wide tool-execution authority. Clients are
trusted peers, not isolated users or sessions. No port publication or browser
CORS grant is added by this kit. For access outside the guest, use an explicit
trusted tunnel; beyond loopback use TLS or a trusted tunnel and approve any
browser origins explicitly. Direct SBX port publication targets the guest network
address and cannot reach this loopback listener. A separately configured,
authenticated non-loopback listener requires separate review; keep any host port
binding loopback unless deliberately exposing it.

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

For readiness failures inspect the reported `server.log` locally, confirm state
ownership/modes and free disk space, and check that the original workspace and
startup inputs are still available. Stale server metadata is recovered under
locks; a mismatched identity or occupied endpoint fails without killing another
process. A settings/custom database path is not a substitute for the managed
state base. Session listing from inside the guest uses:

```sh
/opt/async-agent/docker-agent sessions list --managed-api --managed-api-attach \
  --working-dir /workspace \
  --managed-api-state-dir "$ASYNC_AGENT_KIT_STATE_DIR" --quiet
```

Both descriptor session surfaces use these explicit workspace/state inputs.
Listing uses committed server source and creates no sessions; a dead API yields
an error rather than restarting it or opening a second local database.

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
startup does not prove provider authentication. Actual startup sentinel delivery
and proxy enforcement are unverified; conforming consumers must reject missing
required bindings and withhold unapproved credentials.

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

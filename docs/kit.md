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

The descriptor pins the frontend published with
[`v3.0.0-m.8`](https://github.com/docker/sandbox-kit-spec/releases/tag/v3.0.0-m.8):
`docker/sandbox-kit:3.0.0-m.8@sha256:e6a397771e865625047cf84256a914c7974b3e749d858259b5bc9c653ddb6f0d`.
This frontend emits the expanded descriptor, schema-version and capabilities
annotations on both platform manifests and the top-level index. `task kit`
builds and publishes that index directly, preserving Buildx's provenance defaults;
it does not rewrite the index or run the Kit. Use a compatible Kit v3 consumer.
Do not strip derived package metadata for an older consumer or silently widen
optional permissions.

## Run

Use a **published release containing these sources**. Set `KIT_REF` to its immutable
registry reference, for example `namespace/kagent@sha256:<published-digest>`; the
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

### Persistent API and recovery

The launcher runs the foreground TUI (or `--exec` client) with `--managed-api`.
The Go helper starts or reuses one authenticated API server per sandbox workspace;
concurrent launches serialize startup and verify server identity and readiness.
The daemon has its own process group and private logs, with no terminal stdio.
**Exiting or killing the TUI detaches the client, not accepted server work.** Use
an explicit cancellation action to stop work. Reattach with `--session ID` or
`--session=-1`; omitting these creates a new session.

The optional long-running integration allows background sandbox lifetime when the
host grants it. It is not a supervisor or a guarantee the sandbox stays running.
After a server or sandbox restart, launch again with the same team, models and
startup options to recover durable session state on the retained sandbox disk.
This does **not** promise uninterrupted tools, surviving in-memory execution, or
automatic replay of interrupted side effects. Deleting the sandbox deletes its
local state.

Team instructions, models and referenced team configuration are frozen in a
committed startup snapshot. Different team contents, models or startup options
fail clearly rather than silently reconfiguring or killing existing work; this
also applies after a restart. Use the original inputs or a separate private
`--managed-api-state-dir /home/agent/.cagent/managed-api-other` for a deliberately
separate server and session history. Workspace context files and skill contents
remain live inputs, not frozen instructions: their configured loaders read them
at the usual runtime discovery points. Changing files is not a guarantee that
already-assembled prompts or running tools reload immediately.

Managed mode currently rejects `--fake`, `--fake-stream-delay` and `--record`;
it does not proxy a foreground fake/record provider. For deterministic provider
fixtures, use a separately configured `serve api` daemon and an explicit remote
client outside the kit launcher.

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
`/home/agent/.cagent/managed-api/<canonical-workspace-sha256>/`. A custom
`--managed-api-state-dir BASE` changes the base, not the workspace hash suffix.
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
state base. Session listing from the SBX workspace uses:

```sh
/opt/async-agent/docker-agent sessions list --managed-api --quiet
```

Outside that directory supply `--working-dir /path/to/guest/workspace`; when using
a custom state base supply the same `--managed-api-state-dir BASE`. Listing uses
the active/committed server's source, not a newly selected default team. It may
restart the committed server, but creates no sessions and opens no second local
database. The descriptor uses the default state base; custom bases require the
explicit list command.

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
- **Detached lifetime:** the API server survives TUI exit; optional host support
  permits background sandbox lifetime, not process supervision or uninterrupted
  execution across server/sandbox restarts.
- **Sessions:** prompt `--exec -- PROMPT`, resume `--session ID`, continue
  `--session=-1`. `/opt/async-agent/docker-agent sessions list --managed-api --quiet`
  lists root IDs newest first from the same server-owned history used by resume,
  without creating sessions or opening a second local database.
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
sh -n kit/launch.sh
go test -count=1 ./tests/kit
go vet ./tests/kit
```

Tests cover launcher behavior, team/models, provider grants, optional integration
contracts, synthetic host context/skills discovery, diagnostics, settings and
build-input isolation. They do not prove live SBX permission enforcement, daemon
hook timing, proxy authentication or model entitlement.

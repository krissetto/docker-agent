# Async subagents: native SBX Kit v3

Try the custom Docker Agent from a **prebuilt Kit v3 reference**. The kit bundles
its binary, launcher and the full **hackerspace.yaml** team: you do not need this
source checkout, Python, Go, or local compilation. Async tools and coordination
guidance come from the built-in harness.

## Run a published kit (consumer)

You need an SBX installation supporting Kit v3 and credentials configured in
**SBX's host credential store**. The current source-built kit declares `openai`
and `anthropic` required, with `google` and `github` optional; setup details
are listed below.
You also need pull access if the kit is private. Do not put keys in the workspace,
team YAML, build arguments, or image environment. The proxy retains real keys on
the host and supplies guest sentinel variables. Normal model conversations incur
charges.

### Registration identity and publication status

The legacy **linux/amd64** v3 kit was published as
`christopherpetito053/docker-agent:subagents-reborn`, with immutable reference
`christopherpetito053/docker-agent@sha256:1c2ec35cc46b1886e2774e34249a764c8ae703f4e8e30828fef6ea3de4b28442`.
**Do not use that legacy v3 reference with `sbx run`: it collides with the
built-in `docker-agent`.** Changing only the namespace, tag or digest does not
fix the collision: SBX derives the v3 registration name from the repository's
last path component. `displayName`, `provides`, the descriptor filename and
application entrypoint do not override it; v3 has no `name` field or generic
registration alias.

The corrected **published and remotely verified** repository is
`christopherpetito053/kagent`. It registers the v3 workload as `kagent`.
Staging `christopherpetito234/kagent` remains **unpublished and unverified**.
The published tags are:

| Artifact | Published tag | Immutable digest |
| --- | --- | --- |
| v3 workload | `subagents-reborn` | `sha256:1c2ec35cc46b1886e2774e34249a764c8ae703f4e8e30828fef6ea3de4b28442` |

The existing v3 artifact was copied from a complete local OCI
archive with **no application rebuild**. Remote top-level index bytes match
the originals exactly, including descriptor annotations, platform manifests
and provenance; every referenced blob is accessible in the new repository.
Original `christopherpetito053/docker-agent` references were not modified by
this publication. Their tags returned `not found` during the final registry
recheck; use the verified `kagent` references above.

`ACCESSIBLE_MODEL` is a placeholder for a model your account can use. This
artifact targets spec **v3.0.0-m.5**; see the
[consumer version requirement](#build-inputs-and-validation).
Choose a **fresh, dedicated workspace and sandbox name**:

```sh
KIT_REF='christopherpetito053/kagent@sha256:1c2ec35cc46b1886e2774e34249a764c8ae703f4e8e30828fef6ea3de4b28442'
workspace="$HOME/async-agent-trial-1"
mkdir "$workspace" &&
sbx run --name async-agent-trial-1 "$KIT_REF" "$workspace" -- \
  --model openai/ACCESSIBLE_MODEL
```

The historical published kit's six agents default to OpenAI `gpt-6-astra` with
medium, high or low thinking budgets. If your account lacks access, use the global
`--model openai/ACCESSIBLE_MODEL` argument shown above with an available model;
it overrides **all agents**, including Designer in a current source build. Omit it
to retain the bundled model settings.
The current source-built v3 kit grants the reviewed provider hosts in the
[credential matrix](#provider-credentials-v3); historical immutable references
above remain OpenAI-only. No `--yolo` is enabled.

For initialization **without a model turn**, append `--exec --dry-run` after
`--model openai/ACCESSIBLE_MODEL` in that command. SBX may still require host
credential setup. Neither this check nor a successful image build demonstrates
live proxy authentication or model behavior; live SBX/authentication has not
been tested.

Reattach intentionally to the same sandbox with the same model arguments:

```sh
sbx run --name async-agent-trial-1 -- --model openai/ACCESSIBLE_MODEL
```

### Select your own team at launch

The current source-built launcher accepts `--team PATH` (or `--team=PATH`) as
its **first argument after SBX's `--`**, before any Docker Agent arguments.
For a `team.yaml` in `/path/project`, use a kit release containing this launcher:

```sh
sbx run christopherpetito053/kagent:latest /path/project -- --team ./team.yaml
```

This selects that file directly; you do not need to rename it to
`hackerspace.yaml` or install a workspace override. For a filename with spaces
and an optional global model override:

```sh
sbx run christopherpetito053/kagent:latest /path/project -- \
  --team './my team.yaml' --model openai/ACCESSIBLE_MODEL
```

Paths are resolved **inside the sandbox**: relative paths start at the mounted
workspace, and absolute paths must already be accessible through the workspace
or another SBX mount. The option does not mount arbitrary host files. A missing,
unreadable or non-file path fails with an error; it never silently selects a
different team. The selected YAML is validated by Docker Agent as usual.
The historical immutable kit references above predate this launcher option.

Only the leading `--team` option is consumed. All remaining arguments are passed
unchanged to Docker Agent, including `--model`, `--exec`, `--session ID` and
`--session=-1`. For example, append `--exec -- 'Review this project'` after the
team path for a headless prompt; a literal `--team` after that second `--` is
prompt text, not a launcher option. Supply the same team option when intentionally
reattaching or resuming a session that uses your custom team.

### Bundled team and optional edits

The full supplied configuration preserves the team instructions, model aliases
and tool restrictions:

- The current source-built team has seven agents. `director` uses OpenAI
  `gpt-6-astra` with high thinking. The other OpenAI roles use `gpt-6.1-sol`:
  `root`, `planner` and `reviewer` use high thinking, `engineer` medium, and
  `greppy` low. `designer` uses Anthropic `claude-opus-5-5` with adaptive/high thinking.
- `root` (Shelly) delegates directly to `director`, `engineer`, `designer` and
  `reviewer`. `director` can delegate to all five workers: `greppy`, `planner`,
  `engineer`, `designer` and `reviewer`; its only base tool is filesystem `read_file`.
- Engineer is the default doer for most implementation, debugging and general tasks,
  including UI changes with clear UX/design direction. Designer is used less often,
  for targeted UX advice or web frontend code such as React and interface craft;
  advice-only help is welcome, and not every UI change needs Designer.
- Each team stays on one goal. Short, standalone goals and useful constraints leave
  makers free to execute without micromanagement, polling or duplicate work. Favor
  simplicity, small careful iterations and proportionate checks, not bureaucracy
  or overengineering; safety and correctness still matter. Independent review is
  used only when the user explicitly asks, never as a routine gate.
- Commit subjects are plain single lines and branch names are plain and descriptive.
  Type prefixes such as `fix()`, `fix:`, `feat:`, `fix/` or `feature/` are used only
  when the user explicitly asks.
- `planner` has filesystem `read_file`, shell and todo tools; root, greppy,
  engineer and designer have filesystem, shell and todo; reviewer has filesystem
  and shell. Greppy and Planner keep discovery and analysis read-only.
- Unused Anthropic and DMR model aliases (including a LAN base URL) are retained
  from the supplied file. Current v3 requires the Anthropic credential binding;
  DMR/local LAN connectivity still requires an explicitly reviewed host policy.
  Historical OpenAI-only immutable references are unchanged.

Without `--team`, no configuration copy is required: the launcher uses the bundled
`/opt/async-agent/hackerspace.yaml` when the workspace has no `hackerspace.yaml`.
To prototype instructions, tools or models, optionally create your own Docker
Agent `hackerspace.yaml` in the workspace (the
[bundled configuration](hackerspace.yaml) is an example). That file replaces the
bundled team on a new agent invocation; remove it to use the bundled team again.
A `--model` override still takes precedence over models in the configuration.

## Provider credentials (v3)

The current source-built v3 kit supports **OpenAI, Anthropic and Google Gemini
API credentials**, plus optional GitHub access for `gh` and HTTPS Git. The engine
itself still supports other providers, unchanged, but this kit does not grant
their credentials or network access. Historical immutable references retain the
policies with which they were built; these changes require a new kit build.

| Service | Requirement | Guest sentinel variable | Exact injection hosts | Header |
| --- | --- | --- | --- | --- |
| `openai` | Required | `OPENAI_API_KEY` | `api.openai.com` | `Authorization: Bearer %s` |
| `anthropic` | Required | `ANTHROPIC_API_KEY` | `api.anthropic.com` | `x-api-key: %s` |
| `google` | Optional | `GOOGLE_API_KEY` | `generativelanguage.googleapis.com` | `x-goog-api-key: %s` |
| `github` | Optional | `GH_TOKEN` | `api.github.com`, `github.com` | `Authorization: Bearer %s` |

All four credentials are runtime-only and proxy-managed. The host retains real
credentials; guest environment variables contain sentinels, not usable secrets.
Storing a secret and approving its domain binding are **separate steps**. This
kit does not copy host environment variables, run guest login flows, or write
credential files into the workspace or image.

The current team needs OpenAI for six agents and Anthropic for Designer. Both
credential declarations are required even when `--model` overrides every agent
with one provider. Google and GitHub may be omitted independently; missing GitHub
access does not prevent model use. Credential presence does not automatically
select the model provider. Google uses canonical `GOOGLE_API_KEY`, not a duplicate
`GEMINI_API_KEY` capability. OpenAI's `openai_chatcompletions` and
`openai_responses` variants share the OpenAI binding.

The v3 spec requires resolution to fail for an unsatisfied required credential.
The inspected SBX implementation instead warns and permits launch without it;
its proxy still withholds unapproved credentials. Do not treat successful startup
as authentication proof: configure and approve both required model services.
An optional unbound service supplies no authenticated access, even if a guest
sentinel or its network routes are present.

These declarations cover direct API credentials only—not ChatGPT/Copilot OAuth,
Azure/custom endpoints, Bedrock SigV4/bearer tokens, Vertex ADC, Anthropic WIF,
local DMR/Ollama connectivity, or OAuth refresh. Supporting another mode requires
a separately reviewed kit policy; never bypass proxy isolation with real keys in
guest environment variables, build arguments, or workspace files.

### Host secret setup and domain approval

Use the **host** SBX CLI, with the same host user and `HOME`/`XDG_CONFIG_HOME`
throughout. Prefer sandbox-scoped secrets. The following are setup instructions,
not commands run during kit validation. Choose a fresh name and a published kit
reference containing these changes (`KIT_REF`), plus an intended workspace:

```sh
NAME=async-agent-trial-2
sbx secret set openai --sandbox "$NAME"
sbx secret set anthropic --sandbox "$NAME"
# Optional; omit when GitHub access is not wanted:
sbx secret set github --sandbox "$NAME"
sbx create --name "$NAME" "$KIT_REF" /path/to/workspace
sbx run --name "$NAME"
```

`secret set` accepts masked interactive input, including before the named sandbox
exists. Without `--sandbox`, secrets are global. To use an already authenticated
**host** GitHub CLI as a dynamic source instead of entering a token:

```sh
sbx secret set github --sandbox "$NAME" \
  --command 'gh auth token --hostname github.com'
```

SBX captures the command's output on the host; do not run it separately to print
the token. The host `gh` executable and its configuration must remain outside
sandbox-writable mounts. Avoid inline `--token` values, shell tracing, debug
credential output, and storing tokens in this repository.

Normal interactive creation reviews credential domain bindings. Approve only
intended services and, for GitHub, exactly `api.github.com` and `github.com`.
Stored secrets alone do not authorize injection. SBX persists approval in the
**host** `$XDG_CONFIG_HOME/sbx/credentials.yaml` (default
`~/.config/sbx/credentials.yaml`), separately from secret storage. Noninteractive
creation needs approvals established beforehand; it is not an automatic grant.
Declining Google or GitHub is supported. A missing required model binding can
still allow launch in the inspected SBX, but model calls cannot use that secret.

### GitHub CLI and HTTPS Git

The pinned runtime base already includes `gh` and `git`; no new package or
launcher login hook is needed. `GH_TOKEN` is the proxy sentinel used by `gh` and
takes precedence over `GITHUB_TOKEN`. Leave `GH_HOST` unset or set it to
`github.com`, **not** `api.github.com`. GitHub Enterprise hosts are outside this
policy. Do not use guest `gh auth login`, `gh auth token`, `--show-token`,
`GH_DEBUG=api`, or `gh auth setup-git` to export or persist credentials.

REST and GraphQL use `api.github.com` (GraphQL is `/graphql`). Ordinary `gh`
repository, issue and pull-request operations use the host token's permissions:
select a least-privilege token with the required scopes, fine-grained repository
grants, organization approval/SSO authorization and expiry for the intended work.
Neither the kit nor a sentinel increases those permissions.

For an operator's later checks, `gh api rate_limit` exercises API/proxy
reachability but is public and **does not prove authentication**. Use a read-only
operation on the intended permission-restricted repository, such as
`gh api repos/OWNER/PRIVATE_REPO`, to check the actual grant. `gh auth status`
can report failures for valid GitHub App/installation tokens because `/user`
may return 403; its account, token-prefix and scope diagnostics cannot reliably
describe the real host token behind a sentinel. Judge the intended operation,
not status alone. No such live authenticated checks were performed here.

Use credential-free HTTPS remotes, for example
`https://github.com/OWNER/REPO.git`. The native proxy rewrites GitHub credentials
on bare `github.com` to Basic `x-access-token:<host secret>` for HTTPS Git;
the API uses Bearer auth. No guest token or credential helper setup is required.
SSH remotes (`git@github.com:...`) and SSH authentication are not provided by
this HTTP proxy policy. Optional restricted SSH-agent signing below is a separate
grant, not an SSH transport route or authentication grant.

Only the two exact GitHub hosts above are allowed. Release uploads
(`uploads.github.com`), archives (`codeload.github.com`), raw content, LFS or
release-asset/CDN redirects may fail and need separately reviewed routes; do not
add wildcard credential injection or send the GitHub token to redirected hosts.
Copilot, GHCR and broad GitHub/CDN access are not included.

Schema and offline policy tests verify the four services, requiredness, exact
hosts and proxy-managed declarations. They do not establish live SBX startup,
proxy authentication, provider entitlement, GitHub permissions or model behavior.
No real provider/GitHub credentials were read or used during validation.

## Optional Kit integrations (current source builds)

These additions require a new source build, not the historical immutable m.5
artifact above. `optional: true` lets a host omit an integration without breaking
basic agent use; it does **not** mean automatically approved or enforced by every
host. Permission-bearing features still require host review. Required
`network-policy@1`, `sbx@1`, OpenAI and Anthropic credentials are unchanged;
Google and GitHub remain optional.

### Capability support review

All 18 versioned types in the pinned frontend's spec are reviewed here. Types use
the `com.docker.sandbox/` namespace. "Not requested" means a deliberate workload
choice, not that the spec cannot describe the feature.

| Capability | This workload | Reason / boundary |
| --- | --- | --- |
| `network-policy@1` | Required | Exact reviewed provider/GitHub HTTPS hosts; no added SSH route. |
| `network-policy@2` | Not requested | No concrete method/path restriction to justify migrating the existing policy. |
| `credential@1` | Required / optional | OpenAI and Anthropic required; Google and GitHub optional, runtime proxy-managed. |
| `sbx@1` | Required | Existing shell, user and workspace platform contract. |
| `agent-sessions@1` | Optional | Prompt, resume, continue and actual quiet root-session listing. |
| `agent-context@1` | Optional | Staged `ASYNC_AGENT_KIT.md` guidance, with all seven bundled agents opting into discovery. |
| `agent-skills@1` | Optional | Readonly shared `/home/agent/.agents/skills`; enabled for Shelly, Engineer and Designer only. |
| `git-identity@1` | Optional | Runtime-provided attribution, no imported host Git config or secrets. |
| `ssh-agent@1` | Optional | Runtime only, `unrestricted: false`, `sign: [git]`; no `authenticate` grant. |
| `volume@1` | Optional | Private session/data path `/home/agent/.cagent`, mode `0700`, no arbitrary size request. |
| `long-running@1` | Optional | Detached sandbox lifetime for background coding processes, not agent restart. |
| `lifecycle@1` | Optional | Existing local diagnostics, off by default. |
| `agent-skill@1` | Not requested | No useful bundled skill content; do not advertise an empty skill or rely on discovery symlinks. |
| `kit-registry@1` | Not requested | No nested Kit builds or registry use in the workload. |
| `resources@1` | Not requested | No measured resource constraint for this team. |
| `port@1` | Not requested | No fixed listening service to publish. |
| `usb-device@1` | Not requested | No hardware workload. |
| `privileged@1` | Not requested | No concrete need for elevated privilege. |

### Context and shared skills

The frontend stages [`async-agent-context.md`](async-agent-context.md); a compatible
runtime surfaces it as `ASYNC_AGENT_KIT.md` beside the mounted workspace. All seven
bundled agents declare `add_prompt_files: [AGENTS.md, ASYNC_AGENT_KIT.md]`.
The normal ancestor search discovers both project `AGENTS.md` and the separately
named profile without the project file shadowing the profile. Missing optional
profile content is harmless. No unsupported context `directory` field is used.

Shared skills are mounted readonly at `/home/agent/.agents/skills`, the actual
normal local discovery path. Only **root (Shelly), Engineer and Designer** enable
`skills: true`. Skills add their own reading/fork tools and can contain executable
shell commands; enabling them on Director would bypass its `read_file`-only base
tool contract. Director, Greppy, Planner and Reviewer keep skills disabled to
preserve their restricted or read-only roles. Their base toolsets and all models,
personas and async delegation relationships are unchanged. The bundled YAML has
no extra named toolsets for fork skills to request.

Treat shared skills as untrusted executable input: review them before sharing,
including commands and fork instructions. Readonly mounting prevents guest edits,
not execution or prompt injection. Tool approval still follows application
settings (this image initially enables YOLO); a skill is not authority to broaden
the task. Neither the pinned base nor inspected native v3 runtime sets
`DOCKER_AGENT_KIT_DIR`; if an operator sets it for another integration, it changes
normal skill discovery to that kit's staged directory. The launcher does not
silently clear legitimate environment overrides.

A workspace `hackerspace.yaml` or leading `--team` selection **replaces** the bundled
configuration; it does not inherit these loaders. Custom teams must explicitly
opt into the prompt filenames and, for appropriate roles, local skills. Hosts
omitting shared skills still allow normal agent use and project context.

### Attribution, signing, state and detached lifetime

Optional Git identity provides author/committer attribution only. Restricted
SSH-agent signing is a separate permission: only the `git` signature namespace,
runtime phase, no SSH authentication and no unrestricted socket use. Git must
still select a public signing key (for example through an operator-reviewed local
`user.signingkey` and `gpg.format=ssh`); identity does not configure signing or
identify an approved key. Do not copy private keys or import the host's Git config.
Signing availability and enforcement depend on the host's reviewed agent relay.

The optional volume covers Docker Agent's default Linux **data** directory,
`/home/agent/.cagent` (sessions/database and related app state), not
`/home/agent/.local/share/cagent`. The image precreates it as agent-owned `0700`,
so it is writable when the capability is omitted too. The separately seeded
`/home/agent/.config/cagent` settings directory is not part of this volume.
Custom `--data-dir` or `--session-db` locations are not covered by this mount.

The inspected SBX maps this to a named persistent block volume, supplies its own
default size (currently 512 MiB), and does not apply the descriptor's block-volume
`mode`. A network-disabled Docker fixture proved empty named-volume population
preserves the precreated directory's UID/GID 1000 and mode `0700`, and that the
agent can write it; that is not proof of every SBX backend's mount ownership.
Existing volumes need operator-reviewed ownership rather than recursive startup
chown. Keep SQLite on reliable sandbox-local Linux storage. Retention, cleanup,
recreate behavior and capacity belong to the runtime; this declaration is not a
promise of state surviving sandbox deletion or being reused by a new sandbox.

`long-running@1` maps to detached sandbox behavior in the inspected runtime,
useful for background shell commands or development services after a client
disconnects. It does **not** keep an interactive TUI permanently attached, restart
the application, resume idle subagents, or guarantee autonomous continuation.
If omitted, ordinary interactive/headless launch still works.

## Workspace and runtime boundaries

- Copy only intended project files into the dedicated workspace. All agents
  share files; async sessions do not provide per-worker filesystem isolation.
- The launcher explicitly executes `/opt/async-agent/docker-agent`, forces
  auto-update off, disables telemetry, and passes the SBX workspace as working
  directory. Data/config/cache use Docker Agent's default locations; keep
  transcripts private.
- Fresh sandboxes from current source builds hide the ASCII-art startup banner,
  enable **YOLO** (automatic tool approval), and restore open tabs across app
  restarts. Tab restoration uses the same sandbox's saved state; it does not
  carry tabs into a newly created sandbox.
  The image seeds `/home/agent/.config/cagent/config.yaml` from
  [`user-config.yaml`](user-config.yaml), with agent-owned directories (0700) and
  file (0600), so ordinary **Settings** can change and save these preferences.
  The launcher never writes or resets user settings. Mounted user configuration
  and custom config directories take precedence; non-kit defaults are unchanged.
  Existing sandboxes and historical published images are not changed.
- Current source builds keep SQLite WAL enabled in sandboxes, with FULL
  synchronous durability unchanged. Keep database files on sandbox-local Linux
  storage, not the host-shared workspace or a network filesystem: WAL requires
  reliable shared-memory and file-locking behavior. Sandbox identity alone does
  not identify a database's backing filesystem. This does not relocate existing
  databases or change their persistence lifetime.
- Use a **new workspace and sandbox name** when trying a different kit build or
  digest. Do not adopt old custom-branch databases: async migrations 31–33 are
  not a compatibility promise for those databases. Intentional resume within
  a trial can use `--session ID`; restored children are idle, not automatically
  restarted computations.
- The base includes the agent user (UID 1000), bash, git and CA store, plus
  inherited Docker-in-Docker overhead despite this team not requiring MCP.
  Ensure the agent user can write the trial workspace.
- Current source builds bundle the Go [Task runner](https://taskfile.dev/)
  **3.53.1** at `/usr/local/bin/task`, on the agent user's PATH, for both
  `linux/amd64` and `linux/arm64`. Official release archives are downloaded only
  while building and verified against pinned SHA256 checksums from
  [the upstream release](https://github.com/go-task/task/releases/download/v3.53.1/task_checksums.txt).
  No runtime installation or Go toolchain is required to run `task --version`
  or workspace Taskfiles. Existing sandboxes and historical images are unchanged.
- This is one v3 workload; do not combine it with the built-in Docker Agent kit.

## Maintainers: build and publish

Consumers need only a published immutable reference. Maintainers need [Task](https://taskfile.dev/)
v3, Docker with Buildx and a builder supporting OCI export (for example the
`docker-container` driver), ORAS **1.3.1+**, `jq`, Bash and `shasum`.
Build dependencies are public; registry push access is needed only if you approve
publication. From the **source checkout root**:

```sh
task kit                                      # kagent:local-v3
task kit -- namespace/kagent:trial             # same as build-v3
task kit:build-v3 -- namespace/kagent:trial
```

The default is `kagent:local-v3`. Pass at most one mutable
image reference; an omitted tag becomes `latest`. References like
`namespace/kagent:trial` imply Docker Hub (`docker.io/namespace/kagent:trial`).
Bare names, including the default, expand to `docker.io/library/kagent:…`;
use a namespace you own to publish, not the reserved `library` namespace.
Explicit registries such as `ghcr.io` and `localhost:5000` are preserved;
`docker.io/kagent:tag` also expands to `docker.io/library/kagent:tag`.
Builds, push prompts and generated digest pins all use the expanded reference.
Choose a v3 repository ending in `kagent`, not `docker-agent`, to avoid the
built-in registration collision.
The build context is always the repository root. The target platform defaults to
`linux/amd64`; a comma-separated list builds a multi-platform OCI index. `KIT_PLATFORM` overrides
`DOCKER_DEFAULT_PLATFORM`:

```sh
KIT_PLATFORM=linux/arm64 task kit -- namespace/kagent:arm64
KIT_PLATFORM=linux/amd64,linux/arm64 task kit:build-v3 -- namespace/kagent:latest
```

Each successful build retains a unique directory under ignored `dist/kit/` and
prints its path, tagged reference and immutable digest. The **authoritative OCI
layout** preserves provenance. The exact descriptor, schema-version and
capabilities annotations are promoted locally from the selected platform
manifest to the top-level index before its final digest is computed. Every
requested platform must be present and carry identical Kit annotations; missing
or inconsistent annotations fail the build. Per-platform provenance remains
intact. No registry inspection or repair is needed.

For a single platform, a second cached Buildx invocation loads the same runtime
into local Docker with provenance disabled for Docker image-store portability.
Multi-platform v3 exports skip this load: use the authoritative OCI index for
publication and verify each child platform separately. **A Docker-loaded image
loses kit metadata and is not an SBX kit reference.** Do not publish it with
`docker push`; publication copies only the authoritative OCI layout using ORAS.
The OCI layout itself is not automatically imported into SBX either.

After building, an interactive terminal receives `Push …? [y/N]`. Only `y` or
`yes` (case-insensitive) approves publication. Blank, no, EOF and noninteractive
input keep everything local; piped answers never publish or wait for input.
No registry access occurs before approval except public build dependency fetches.
On approval, the final printed immutable kit reference can be shared with
consumers using the workflow above. Build/push failures stop immediately and
leave the local outputs available for inspection.

All kit definitions, runtime assets, documentation and tests live in `kit/`.
The `kit/async-agent.yaml` file is the v3 descriptor. Its ordinary companion
`kit/async-agent.dockerfile` compiles Linux statically (`no_audio`, `xx-verify
--static`) and installs the custom binary, `hackerspace.yaml`, `launch.sh`,
initial `user-config.yaml`, and the checksum-pinned Task executable and license.
The Kit frontend separately stages `async-agent-context.md`; no duplicate
Dockerfile COPY of that profile is needed.

### Local source builds

Build local edits from the **source checkout root**, keeping the repository as
Buildx's context and selecting `kit/async-agent.yaml` with `-f`, as shown in
[Build inputs and validation](#build-inputs-and-validation). Do not build with
`kit/` as the context: the recipe needs the root Go module, `cmd/` and `pkg/`.
The descriptor's `dockerfile: ./async-agent.dockerfile` is resolved relative to
its own directory; Dockerfile `COPY` paths remain relative to the root context.
Unlike the companion path, authored `contentFile: ./kit/async-agent-context.md`
is relative to the **root build context**. The frontend stages and rewrites it to
`/usr/share/sandbox/kit/async-agent/async-agent-context.md` in the published
descriptor. Annotation regeneration must use the frontend-matching spec below,
expand build arguments and preserve this in-image path, not publish the authored
source path. Inspect that staged file in each platform image before publishing.

The current SBX local-source loader **does not support this nested source layout**:
it scans only the supplied directory for a descriptor and uses that same
directory as the build context. Passing the checkout root misses the nested
descriptor; passing `kit/` loses the Go sources and would register the name
`kit`, not `kagent`. A descriptor file is not a supported local kit reference.
Do not work around this with root symlinks: the source hasher does not traverse
a root directory symlink, so edits can incorrectly reuse a cached build.

Use Buildx for local build-only validation. To launch changed sources in SBX,
publish the resulting kit to a repository ending in `kagent` and use its
immutable reference with the consumer workflow above. A local OCI export is
not automatically imported into SBX. The checkout's directory name does not
matter for this explicit Buildx workflow.

### Kit metadata and optional diagnostics

The kit's packaging version defaults to `0.1.0`; it is **not** a Docker Agent
release version. `--build-arg kitVersion=0.1.1` changes the versioned
`async-agent-kit` provide, descriptor version and `com.docker.async-agent.kit.version`
image label together. The frontend also derives the installed Debian package
provides from the image; these entries are retained, not rewritten for an older
consumer. `Apache-2.0` identifies this kit/application's license, not a claim
that every inherited OS package has that license.

The required, permission-free `sbx@1` capability states the image's existing
shell, user and workspace contract. `agent-sessions@1` describes the existing
CLI: a headless prompt is `--exec -- PROMPT`, resume is `--session ID`, and
continue is `--session=-1`. It is optional. Its `list` is a **complete command**,
not an entrypoint tail: `/opt/async-agent/docker-agent sessions list --quiet`.
The command also accepts `-q`; it prints full root-session IDs only, one per
line, newest first, without a provider/model startup. Delegated children are
omitted; an empty store prints nothing. Listing does not validate complete stored
history or team compatibility; resume still performs those checks. It honors the
application config/data/database locations and must run in the same sandbox
state context as resume. Prompt/resume/continue remain unchanged.
Prompts stay single argv values, including a leading dash or shell metacharacter.
These session declarations do not change the team or enable `--yolo`.

Optional lifecycle diagnostics default to **off**. With a compatible SBX,
`--kit-arg diagnostics=on` opts in to markers beneath
`/home/agent/.local/state/async-agent-kit/probe`, never in the workspace. The
install hook appends one `installed` line **per invocation**; startup atomically
replaces the same `ready` marker and tolerates repeated runs. Both run as
`agent`, declare only `ASYNC_AGENT_KIT_DIAGNOSTICS` in their environment
allowlist, and perform no network or credential operations. Off means no
marker directory is created. A host may skip this optional capability. Local
shell tests exercise those behaviors; they do **not** demonstrate that a daemon
runs install exactly once per create or startup after every boot.

### Build inputs and validation

- Companion-specific `.dockerignore` permits Go source, required embedded assets
  and this kit's deliberate runtime assets plus the staged context Markdown.
  Tests, fixtures, `.git`, dotenv files,
  databases and unrelated artifacts are excluded. Source edits, including
  untracked matching Go files, are built. Review inputs: hardcoded secrets in
  legitimate source files cannot be automatically filtered out. These exclusions
  apply to the explicit Buildx workflow; do not pass the checkout to an SBX
  source builder (see the nested-layout limitation above).
- Version reports `dev` / `local-source`, **not a clean-commit claim**. Frontend
  and runtime base are digest-pinned; builder images/packages follow the root
  Dockerfile conventions and are not a fully hermetic dependency lock.

Build-only checks from the source root (no publication or provider calls):

```sh
docker buildx build . -f kit/async-agent.yaml --platform linux/amd64 \
  --output type=oci,dest=/tmp/async-agent.oci.tar
go test ./kit
```

The relocated descriptor and its companion Dockerfile have passed root-context
Buildx builds with `--output type=cacheonly`; the v3 build compiled and statically
verified the binary and copied both relocated runtime assets. These checks
used public build dependencies and did not publish or load an image into SBX.

Earlier native Buildx OCI exports and the bundled launcher's `--exec --dry-run`
with an OpenAI model override were tested locally; the launcher checks used a
fake key with networking disabled (not an SBX proxy/authentication test). The
OCI archive is a standard image export, **not automatically imported into SBX**.
Publish and use the consumer reference workflow for SBX launch; direct
local-source loading is not supported for this nested layout.

The current source descriptor pins `docker/sandbox-kit:3` at
`sha256:a8659fa579de7e8d8dc5b5712feba5efdabceb953279106e9c770d2cd8ca52f1`,
frontend source revision `34df175ec8696ab0a1ad76116b358595552b6794` (reported
with a dirty build marker). The matching spec module is exactly
`v3.0.0-m.6.0.20260929191907-34df175ec869`, pinned in `spec-v3-tests`.
It supports the bounded SSH, Git identity and detached-lifetime fields used here;
these are not supported by the historical m.5 frontend. This revision is not the
later m.7 tag, and still-newer default-branch schema fields such as context
`directory` are deliberately not used. The runtime base digest is unchanged.
This frontend publishes `vnd.docker.sandbox.kit.descriptor` on each platform image
manifest. The `docker/runtime-kit:3` frontend and `com.docker.runtime/*`
capabilities are a different contract, not interchangeable names.

**Consumer version requirement:** historical references require full m.5
support; new source builds require the pinned descriptor grammar and the
requested integrations above (inspected sibling v3 runtime uses m.7). Optional
features may be skipped, but must not be silently widened, especially restricted
SSH signing. Successful schema validation does not prove runtime enforcement.
An earlier local CLI embedding `sandbox-kit-spec/v3 v3.0.0-m.3`
rejected `deb/containerd.io@2.3.3` among the automatically derived provides;
updating the consumer is required for that separate compatibility issue.
The CLI used for the historical identity investigation,
`/Users/krissetto/dev/ai-dev/sandboxes/bin/sbx`, reports revision
`6be7806b86922201e946911910e9990f467e61ca` (dirty source build). Its loader and
registration pathway confirm the legacy v3 `docker-agent` name collision and
accept the unique `kagent` name in offline tests. A frontend update,
`displayName` edit or application rebuild alone cannot fix reference identity.
No installed CLI or daemon was changed for this identity fix, and no package
provides were stripped or older frontend substituted.

For the historical immutable artifact, the official m.5 artifact TCK passed all
15 checks with no warnings on both the exported workload and final published index, including actual image
user/shell/passwd checks and derived package inventory. The published amd64
child is `sha256:1f1ea04dff583a977c40a5fd2d13333aceb8f85fd0e736a82bde8645fc7dc21d`;
index annotation promotion leaves that child and provenance unchanged.
Some checks for undeclared optional features are vacuous: this is not a claim
that every spec capability is exercised or that a sandbox runtime conforms.
Network-disabled image checks confirmed UID/GID 1000, amd64, unchanged
application/team/launcher hashes, and initialization without a model turn.
The base digest supports linux/amd64 and linux/arm64.

The offline fixtures cover all 18 versioned capability types in the pinned
frontend spec, alternative authoring forms, arguments and composition. The current
workload uses ten non-credential capabilities plus four credential declarations;
all integrations except the essential network/SBX contract and two model
credentials are optional. Loader fixtures exercise actual project context, the
runtime-sibling profile and shared skill discovery with isolated HOME/config/data
and no provider requests, including custom-team opt-in and restricted-role tools.
Fixture validation is not runtime enforcement evidence. Run the separately
pinned test module without adding the spec dependency to Docker Agent:

```sh
(cd kit/spec-v3-tests && TMPDIR=/private/tmp go test ./...)
```

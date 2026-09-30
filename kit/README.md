# Async subagents: native SBX Kit v3

Try the custom Docker Agent from a **prebuilt Kit v3 reference**. The kit bundles
its binary, launcher and the full **hackerspace.yaml** team: you do not need this
source checkout, Python, Go, or local compilation. Async tools and coordination
guidance come from the built-in harness.

For the separately published Kit v2 equivalent, see [Kit v2](#kit-v2-equivalent)
below; its explicit v2 name already avoids the built-in agent collision.

## Run a published kit (consumer)

You need an SBX installation supporting Kit v3 and the credentials for the
providers you actually use configured in **SBX's host credential store**. The
bundled team uses service `openai`; alternative providers are listed below.
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
| v2 kit | `v2-kit-subagents-reborn` | `sha256:b0be556365b6283520d312d4fdd33a37bce528602831a56ed272a87f5c43cac5` |
| v2 runtime image (not a kit reference) | `v2-runtime-subagents-reborn` | `sha256:e1410a8adf0af4349483b876ae18b3a9bdccdd4eb8c175e95fc137bd834fa7ed` |

The existing v3 and runtime artifacts were copied from complete local OCI
archives with **no application rebuild**. Remote top-level index bytes match
the originals exactly, including descriptor annotations, platform manifests
and provenance; every referenced blob is accessible in the new repository.
The v2 kit was republished with `name: kagent` and its runtime reference changed
to this repository; its provider permissions and credential policy are unchanged.
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

The bundled six agents default to OpenAI `gpt-6-astra` with medium, high or low
thinking budgets. If your account lacks access, use the global
`--model openai/ACCESSIBLE_MODEL` argument shown above with an available model;
it overrides **all six agents**. Omit it to retain the bundled model settings.
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

### Bundled team and optional edits

The full supplied configuration preserves the team instructions, model aliases
and tool restrictions:

- `root` (Shelly) delegates to `director`, `implementer` and `reviewer`.
- `director` delegates to `greppy`, `planner` and `implementer`; its only base
  tool is filesystem `read_file`.
- `planner` has filesystem `read_file`, shell and todo tools; root, greppy and
  implementer have filesystem, shell and todo; reviewer has filesystem and shell.
- Unused Anthropic and DMR model aliases (including a LAN base URL) are retained
  from the supplied file. Current v3 supports the optional Anthropic host binding;
  DMR/local LAN connectivity still requires an explicitly reviewed host policy.
  Historical OpenAI-only immutable references are unchanged.

No configuration copy is required: the launcher uses the bundled
`/opt/async-agent/hackerspace.yaml` when the workspace has no `hackerspace.yaml`.
To prototype instructions, tools or models, optionally create your own Docker
Agent `hackerspace.yaml` in the workspace (the
[bundled configuration](hackerspace.yaml) is an example). That file replaces the
bundled team on a new agent invocation; remove it to use the bundled team again.
A `--model` override still takes precedence over models in the configuration.

## Provider credentials (v3)

The current source-built v3 kit supports **only OpenAI, Anthropic and Google
Gemini API credentials**. The engine itself still supports other providers,
unchanged, but this kit does not grant their credentials or network access.
Historical immutable references retain the policies with which they were built.

| Provider | Host service | Guest sentinel variable | Only allowed injection host | Header |
| --- | --- | --- | --- | --- |
| OpenAI | `openai` | `OPENAI_API_KEY` | `api.openai.com` | `Authorization: Bearer %s` |
| Anthropic | `anthropic` | `ANTHROPIC_API_KEY` | `api.anthropic.com` | `x-api-key: %s` |
| Google Gemini | `google` | `GOOGLE_API_KEY` | `generativelanguage.googleapis.com` | `x-goog-api-key: %s` |

All three credential capabilities are optional and proxy-managed. An unbound
service can be skipped; this does **not** detect or automatically select the
model's provider. Every service you bind is available in the sandbox. Bind only
the services you need, preferably sandbox-scoped rather than globally, using
`sbx secret set <service>` and your SBX version's interactive secret input and
scoping options. The host retains real credentials; guest environment variables
contain sentinels. Host environment variables are not imported automatically.

The default bundled team still selects OpenAI. To override all six agents, use
`--model anthropic/ACCESSIBLE_MODEL` or `--model google/ACCESSIBLE_MODEL` and bind
that service. Google uses canonical `GOOGLE_API_KEY`; no duplicate
`GEMINI_API_KEY` capability is needed. OpenAI's `openai_chatcompletions` and
`openai_responses` API variants share the OpenAI binding.

These declarations cover direct API credentials only—not ChatGPT/Copilot OAuth,
Azure/custom endpoints, Bedrock SigV4/bearer tokens, Vertex ADC, Anthropic WIF,
local DMR/Ollama connectivity, or OAuth refresh. No broad-provider extension
examples are bundled. Supporting another mode requires a separately reviewed
kit policy; do not work around proxy isolation by placing real keys in image
environment variables, build arguments, or workspace files.

Schema and offline tests verify this exact three-service policy. They do not
establish live SBX startup, proxy authentication, provider entitlement, or model
behavior. No real provider credentials were read or used during validation.

## Workspace and runtime boundaries

- Copy only intended project files into the dedicated workspace. All agents
  share files; async sessions do not provide per-worker filesystem isolation.
- The launcher explicitly executes `/opt/async-agent/docker-agent`, forces
  auto-update off, disables telemetry, and passes the SBX workspace as working
  directory. Data/config/cache use Docker Agent's default locations; keep
  transcripts private.
- Use a **new workspace and sandbox name** when trying a different kit build or
  digest. Do not adopt old custom-branch databases: async migrations 31–33 are
  not a compatibility promise for those databases. Intentional resume within
  a trial can use `--session ID`; restored children are idle, not automatically
  restarted computations.
- The base includes the agent user (UID 1000), bash, git and CA store, plus
  inherited Docker-in-Docker overhead despite this team not requiring MCP.
  Ensure the agent user can write the trial workspace.
- This is one v3 workload; do not combine it with the built-in v2 Docker Agent
  kit.

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
task kit:build-v2 -- namespace/kagent:trial-v2   # kit + trial-v2-runtime
```

Defaults are `kagent:local-v3` and `kagent:local-v2`. Pass at most one mutable
image reference; an omitted tag becomes `latest`. References like
`namespace/kagent:trial` imply Docker Hub (`docker.io/namespace/kagent:trial`).
Bare names, including the defaults, expand to `docker.io/library/kagent:…`;
use a namespace you own to publish, not the reserved `library` namespace.
Explicit registries such as `ghcr.io` and `localhost:5000` are preserved;
`docker.io/kagent:tag` also expands to `docker.io/library/kagent:tag`.
Builds, push prompts and generated digest pins all use the expanded reference.
Choose a v3 repository ending in `kagent`, not `docker-agent`, to avoid the
built-in registration collision.
The build context is always the repository root. The single target platform
defaults to `linux/amd64`; `KIT_PLATFORM` overrides `DOCKER_DEFAULT_PLATFORM`:

```sh
KIT_PLATFORM=linux/arm64 task kit -- namespace/kagent:arm64
```

Each successful build retains a unique directory under ignored `dist/kit/` and
prints its path, tagged reference and immutable digest. The **authoritative OCI
layout** preserves provenance. For v3, the exact descriptor, schema-version and
capabilities annotations are promoted locally from the selected platform
manifest to the top-level index before its final digest is computed. Missing
annotations fail the build; no registry inspection or repair is needed.

A second cached Buildx invocation loads the same runtime into local Docker with
provenance disabled for Docker image-store portability. **That local Docker image
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
--static`) and installs the custom binary, `hackerspace.yaml` and `launch.sh`.

### Local source builds

Build local edits from the **source checkout root**, keeping the repository as
Buildx's context and selecting `kit/async-agent.yaml` with `-f`, as shown in
[Build inputs and validation](#build-inputs-and-validation). Do not build with
`kit/` as the context: the recipe needs the root Go module, `cmd/` and `pkg/`.
The descriptor's `dockerfile: ./async-agent.dockerfile` is resolved relative to
its own directory; Dockerfile `COPY` paths remain relative to the root context.

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
continue is `--session=-1`. No unsupported session-list command is advertised.
Prompts stay single argv values, including a leading dash or shell metacharacter.
These declarations do not change the six-agent team or enable `--yolo`.

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
  and this team's two runtime files. Tests, fixtures, `.git`, dotenv files,
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

The relocated descriptor and both Dockerfiles have passed root-context Buildx
builds with `--output type=cacheonly`; the v3 build compiled and statically
verified the binary and copied both relocated runtime assets. These checks
used public build dependencies and did not publish or load an image into SBX.

Earlier native Buildx OCI exports and the bundled launcher's `--exec --dry-run`
with an OpenAI model override were tested locally; the launcher checks used a
fake key with networking disabled (not an SBX proxy/authentication test). The
OCI archive is a standard image export, **not automatically imported into SBX**.
Publish and use the consumer reference workflow for SBX launch; direct
local-source loading is not supported for this nested layout.

The descriptor pins the latest released sandbox-kit spec frontend,
`v3.0.0-m.5` (source `6ddbd62d5fcb3ef7db0eb73fdf34fb04dc4281f4`), as
`docker/sandbox-kit:3@sha256:11bb68806aef6a68d45c10c0c3b3dc86c2f794a296b5a933661d9dadf760b753`.
The published frontend binary reports that revision with a dirty build marker.
Remote release/tag checks on 2026-09-21 found no newer v3 release; default-branch
HEAD was `3d945d2d045fe59301f1b5429cf47763b01b6c94`, with no production spec or
frontend changes beyond m.5. This frontend publishes
`vnd.docker.sandbox.kit.descriptor` on each platform image manifest and uses
`com.docker.sandbox/network-policy@1` and `com.docker.sandbox/credential@1`.
The `docker/runtime-kit:3` frontend and `com.docker.runtime/*` capabilities
are a different contract, not interchangeable names.

**Consumer version requirement:** use an SBX build that supports the full
m.5 descriptor, including derived package-name grammar and the declared
capabilities. An earlier local CLI embedding `sandbox-kit-spec/v3 v3.0.0-m.3`
rejected `deb/containerd.io@2.3.3` among the automatically derived provides;
updating the consumer is required for that separate compatibility issue.
The current explicitly requested CLI,
`/Users/krissetto/dev/ai-dev/sandboxes/bin/sbx`, reports revision
`6be7806b86922201e946911910e9990f467e61ca` (dirty source build). Its loader and
registration pathway confirm the legacy v3 `docker-agent` name collision and
accept the unique `kagent` name in offline tests. A frontend update,
`displayName` edit or application rebuild alone cannot fix reference identity.
No installed CLI or daemon was changed for this identity fix, and no package
provides were stripped or older frontend substituted.

The official m.5 artifact TCK passed all 15 checks with no warnings on both the
exported workload and final published index, including actual image
user/shell/passwd checks and derived package inventory. The published amd64
child is `sha256:1f1ea04dff583a977c40a5fd2d13333aceb8f85fd0e736a82bde8645fc7dc21d`;
index annotation promotion leaves that child and provenance unchanged.
Some checks for undeclared optional features are vacuous: this is not a claim
that every spec capability is exercised or that a sandbox runtime conforms.
Network-disabled image checks confirmed UID/GID 1000, amd64, unchanged
application/team/launcher hashes, and initialization without a model turn.
The base digest supports linux/amd64 and linux/arm64.

The offline fixtures cover all 14 m.5 capability types, alternative authoring
forms, arguments and composition; the runnable workload uses four non-credential capabilities plus the optional provider bindings above.
Fixture validation is not runtime enforcement evidence. Run the separately
pinned test module without adding the spec dependency to Docker Agent:

```sh
(cd kit/spec-v3-tests && TMPDIR=/private/tmp go test ./...)
```

## Kit v2 equivalent

The published **linux/amd64** v2 kit is
`christopherpetito053/kagent:v2-kit-subagents-reborn`. It is a genuine
`schemaVersion: "2"` OCI kit artifact, not a retagged v3 image. Its explicit
`name: kagent` survives loading independently of the OCI repository basename.
The local [spec.yaml](v2/spec.yaml) and remote artifact have the same effective
spec and pin the separate runtime image in the same repository:
`christopherpetito053/kagent:v2-runtime-subagents-reborn` resolves to
`sha256:e1410a8adf0af4349483b876ae18b3a9bdccdd4eb8c175e95fc137bd834fa7ed`.

The legacy `christopherpetito053/docker-agent:v2-kit-subagents-reborn`
(`christopherpetito053/docker-agent@sha256:b335ad9f1cc5905a4f34d6884a2ec0a3e27a10a5b1fa872239f366e8a0dbb7ec`)
was not modified by this publication; its last verified descriptor had
`name: async-agent-subagents-reborn` and runtime
`christopherpetito053/docker-agent:v2-runtime-subagents-reborn`. Its tag now
returns `not found`. Compared with that last verified legacy kit, only the new
kit's name and runtime repository changed.

The historical published runtime derives from the immutable published v3
application image in `async-agent-v2.dockerfile`. New Task builds compile source
as described below. The binary, six-agent team, launcher, non-root user
and environment are unchanged. Only the image entrypoint becomes
`/usr/bin/tini --` with an empty CMD so v2's sandbox idle keeper can start;
`sandbox.entrypoint` separately runs `/opt/async-agent/launch.sh` for the agent
session. No built-in Docker Agent kit, setup scripts or extra providers are
inherited by the descriptor.

### Run the published v2 kit

Use an SBX release that supports this v2 schema, configure the `openai` service
in SBX's host credential store, and choose a fresh workspace and sandbox name:

```sh
KIT_REF='christopherpetito053/kagent@sha256:b0be556365b6283520d312d4fdd33a37bce528602831a56ed272a87f5c43cac5'
workspace="$HOME/async-agent-v2-trial-1"
mkdir "$workspace" &&
sbx run --name async-agent-v2-trial-1 "$KIT_REF" "$workspace" -- \
  --model openai/ACCESSIBLE_MODEL
```

Replace `ACCESSIBLE_MODEL` with a model your account can use. The same launcher
arguments, global model override, optional workspace `hackerspace.yaml`, and
workspace boundaries described above apply. Append `--exec --dry-run` for
initialization without a model turn. Use the **kit** reference with `sbx run`,
not the runtime image reference.

The v2 descriptor grants only `api.openai.com` and declares the same
proxy-managed `OPENAI_API_KEY` sentinel plus `Authorization: Bearer` injection.
SBX host policy may additionally allow registry mirrors in either kit format.
Schema validation does not prove third-party consent or live authentication;
no live SBX proxy/authentication or model call was tested. Local runtime smoke
checks used a fake key with networking disabled. No `--yolo` is enabled.

### Build and publish v2

```sh
task kit:build-v2                              # kagent:local-v2
task kit:build-v2 -- namespace/kagent:trial-v2
```

The requested reference is the **v2 kit artifact**, not a Docker runtime image.
The runtime uses the same repository and the kit tag plus `-runtime` (for example
`namespace/kagent:trial-v2-runtime`). Tags whose runtime suffix would exceed
Docker's 128-character limit are rejected before building.

This workflow compiles current source through `kit/async-agent.dockerfile`'s
`runtime-v2` target, sharing the v3 application/runtime stages. It retains
`/usr/bin/tini --` and an empty CMD for v2's idle keeper; the kit's
`sandbox.entrypoint` starts the launcher. The default final Dockerfile target
still uses the v3 launcher. The historical `async-agent-v2.dockerfile` and tracked
`v2/spec.yaml` remain unchanged published fixtures, not the source-build recipe.

The durable output directory contains `runtime/` and `kit/` OCI layouts and a
generated `spec.yaml`. Only `sandbox.image` changes in this generated copy: it
pins the requested repository to the **runtime's OCI top-level digest**, not a
Docker image/config ID. ORAS packs the YAML as the config with media type
`application/vnd.docker.sandbox.kit.v2.spec+yaml` and artifact type
`application/vnd.docker.sandbox.kit.v2`, using OCI 1.1's canonical empty `{}`
layer (no payload tar). The kit is never loaded as a Docker image.

Approval publishes the runtime OCI layout **first**, then the kit OCI layout.
Use the resulting immutable **kit** reference with `sbx run`, never the runtime
reference. Both formats intentionally register as `kagent`; do not combine them
in one sandbox. Building or publishing is not a live SBX authentication/model
test and does not require provider credentials.

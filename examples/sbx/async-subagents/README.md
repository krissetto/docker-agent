# Async subagents: native SBX Kit v3

Try the custom Docker Agent from a **prebuilt Kit v3 reference**. The kit bundles
its binary, launcher and the full **hackerspace.yaml** team: you do not need this
source checkout, Python, Go, or local compilation. Async tools and coordination
guidance come from the built-in harness.

## Run a published kit (consumer)

You need an SBX installation supporting Kit v3 and OpenAI credentials configured
in **SBX's host credential store** (service `openai`). You also need pull access
to the kit's registry if it is private. Do not put keys in the workspace, team
YAML or build arguments. The proxy injects the real key only into requests to
`api.openai.com`; the agent receives a sentinel. Normal model conversations
incur charges.

**No published reference is available yet.** Obtain the reference from the
maintainer and set `KIT_REF` below to its actual digest-pinned reference.
`REGISTRY/REPO@sha256:DIGEST` and `ACCESSIBLE_MODEL` are placeholders, not a
downloadable kit or a model name; replace both before running.

From any directory, choose a **fresh, dedicated workspace and sandbox name**:

```sh
KIT_REF='REGISTRY/REPO@sha256:DIGEST' # Replace with the maintainer's reference.
workspace="$HOME/async-agent-trial-1"
mkdir "$workspace" &&
sbx run --name async-agent-trial-1 "$KIT_REF" "$workspace" -- \
  --model openai/ACCESSIBLE_MODEL
```

The bundled six agents default to OpenAI `gpt-6-astra` with medium, high or low
thinking budgets. If your account lacks access, use the global
`--model openai/ACCESSIBLE_MODEL` argument shown above with an available model;
it overrides **all six agents**. Omit it to retain the bundled model settings.
Runtime egress grants **only api.openai.com**. No `--yolo` is enabled.

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
  from the supplied file, **not enabled by this kit**. Only OpenAI host
  credentials and egress are granted; changing to another provider requires
  a different kit policy, not just a team edit.

No configuration copy is required: the launcher uses the bundled
`/opt/async-agent/hackerspace.yaml` when the workspace has no `hackerspace.yaml`.
To prototype instructions, tools or models, optionally create your own Docker
Agent `hackerspace.yaml` in the workspace (the
[bundled configuration](hackerspace.yaml) is an example). That file replaces the
bundled team on a new agent invocation; remove it to use the bundled team again.
A `--model` override still takes precedence over models in the configuration.

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

Consumers need only the resulting reference. Maintainers need the source
checkout, Docker Buildx, public build dependencies and push access to their
chosen registry repository. The publication commands below are documentation
only; no kit has been pushed.

From the **docker-agent source checkout root**, set your registry/repository
and tag, then build, publish and inspect the registry digest:

```sh
REPO='REGISTRY/REPO' # Replace with your registry and repository.
TAG='async-subagents-hackerspace-1'
docker buildx build . -f async-agent.yaml --platform linux/amd64 -t "$REPO:$TAG" --push &&
docker buildx imagetools inspect "$REPO:$TAG"
```

Copy the top-level `Digest:` from that inspection (not a platform child digest):

```sh
DIGEST='sha256:REPLACE_WITH_INSPECTED_DIGEST'
KIT_REF="$REPO@$DIGEST"
printf '%s\n' "$KIT_REF"
```

Share that immutable `KIT_REF` with consumers rather than a mutable tag. They
need neither the source nor a configuration copy. Publish for their platform:
amd64 is shown above; arm64 is also a build target.

The root `async-agent.yaml` is the v3 descriptor. Its ordinary companion
`async-agent.dockerfile` compiles Linux statically (`no_audio`, `xx-verify
--static`) and installs the custom binary, `hackerspace.yaml` and `launch.sh`.

### Optional local source trial

To test local edits without publishing, run from the source checkout root,
using a fresh name and workspace for each build revision:

```sh
workspace="$HOME/async-agent-source-trial-1"
mkdir "$workspace" &&
SBX_KIT_BUILDER=host sbx run --name async-agent-source-trial-1 . "$workspace" -- \
  --model openai/ACCESSIBLE_MODEL
```

The bundled team works here too. `SBX_KIT_BUILDER=host` preserves the pinned
frontend; the alternative builder may override it.

### Build inputs and validation

- Companion-specific `.dockerignore` permits Go source, required embedded assets
  and this team's two runtime files. Tests, fixtures, `.git`, dotenv files,
  databases and unrelated artifacts are excluded. Source edits, including
  untracked matching Go files, are built. Review inputs: hardcoded secrets in
  legitimate source files cannot be automatically filtered out. SBX still reads
  regular source-directory files (except `.git`) for cache hashing, even when
  ignored by the build. Force the host builder for local source trials: the
  sandbox builder can transfer the whole source directory without these
  exclusions.
- Version reports `dev` / `local-source`, **not a clean-commit claim**. Frontend
  and runtime base are digest-pinned; builder images/packages follow the root
  Dockerfile conventions and are not a fully hermetic dependency lock.

Build-only checks from the source root (no publication or provider calls):

```sh
docker buildx build . -f async-agent.yaml --platform linux/amd64 \
  --output type=oci,dest=/tmp/async-agent.oci.tar
go test ./examples/sbx/async-subagents
```

The native Buildx OCI build and the bundled launcher's `--exec --dry-run` with
an OpenAI model override have been tested locally (network disabled, fake key;
not an SBX proxy/authentication test). The OCI archive is a standard image
export, **not automatically imported into SBX**. Use the optional source
`sbx run` workflow for native build/import/launch, or publish and use the
consumer reference workflow.

Pins were checked against canonical runtime-kits **v0.2.0** commit
`31c5baa734ac176ff7c9e3945bb6ddc00dfbd669` and sandboxes main
`93bebe4c1ec66fc1731c04f6d99951326421dbb0` on 2026-09-12. The descriptor pins the
verified `docker/runtime-kit:3` digest; `v0.2.0` is not a frontend image tag.
The base digest supports linux/amd64 and linux/arm64.

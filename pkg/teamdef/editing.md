# Controlled team-definition editing core

The `teamdef.Editor` is an isolated in-memory core, not an active runtime or tool API.
A trusted caller pins a selected root YAML source, full team-build validator,
authority mode, and any persistence/export capabilities when constructing it.
Normal `config.Load` behavior is unchanged and does not acquire editor restrictions.
Authority is constructor-only: empty mode defaults to `restricted`; `full` must
come from a trusted harness grant; `off` and unknown values reject construction.
No authority field is read from editable YAML or an edit operation.

Inspection is actor-scoped and returns only agents reachable in the actor's
pinned baseline closure. Validation also requires the pinned actor identity and
returns only pass/fail status.

Every patch is compare-and-swap against an immutable source revision and must
carry actor identity (`TeamID`, baseline `SourceRevision`, stable `AgentKey`)
from the session's pinned team. `SourceRevision` is the immutable editor epoch,
not the current head: an actor can continue to edit after its own commit using
the new head CAS token. A user reload creates a new editor epoch and invalidates
old actor refs. The target must be reachable in the actor's
baseline local `sub_agents`, `handoffs`, `subagents`, or `force_handoff` graph.
The graph and actor identity are captured once and never reset by edits or
persistence. The runtime must check them against its authoritative pinned team
identity; the editor is not an identity provider.

Supported changes are instruction, description, welcome message, rename,
primary/fallback model references, and references on an existing
`model_picker`. Model references can only be selected from that target's
baseline model/fallback/model-picker set. Cache-enabled baseline classes reject
edits to model-facing text and model references because the existing response
cache is keyed only by the user question. Rename retains or creates the prior
key as stable ID and rewrites local delegation edges without changing their
stable-identity graph. Renaming the default/root agent or a team with external
agent references is rejected.

Exact clones and safe removals are supported for the actor's reachable baseline
classes. Cloning preserves the exact capability profile, assigns a fresh stable
ID, cannot clone harness/cache/team-edit agents or external delegation teams,
and adds no inbound edges; model-picker references are bounded by the source
class. Removal rejects default/actor agents, force-handoff targets, and any
operation that empties an existing delegation list; it prunes the removed
agent from every permitted inbound reference. Every resulting class's transitive
delegation reach remains within that class's pinned baseline closure, and the
presence/absence of derived delegation lists does not change. `Apply` returns an
`EditResult` with operation, current revision, original baseline epoch, target
stable ID/key and immutable capability-class identity; runtime team builders
should use that receipt and must never derive capabilities from tool arguments.

The editor
also rejects non-latest YAML, HCL/non-YAML inputs, includes/flavors,
instruction files, shared `use_*` references, aliases/anchors/merge keys,
multiple documents, duplicate or non-string mapping keys, and ambiguous field
shapes. Existing comments and unresolved `${...}` author placeholders are kept
in the source revision; credentials and resolved environment values are never
loaded into inspection projections. Full `config.Load` and the injected build
validator must pass before any revision is committed.

Remote direct latest `.yaml`/`.yml` YAML may be captured in memory and exported
but is never persisted. Local sources are also read-only by default; the trusted
caller must explicitly provide `WritablePath` via `NewEditableSourceWithOptions`
and that path must exactly match the captured direct regular file. A tool must
never accept this capability from user arguments. `NewEditableSourceWithExportRoot`
requires an explicit trusted export directory; `ExportNew` confines creation to
that directory and rejects symlink path components. `Persist` only targets the original regular local file and
checks its captured content hash before atomic replacement while retaining its original POSIX permission bits
(Windows uses inherited ACLs, following `pkg/atomicfile`). The
hash recheck narrows external-writer conflicts, but cannot provide a true
filesystem compare-and-swap against non-cooperating writers racing between the
check and replace. The in-memory revision history is capped at 32 entries; callers must retain/use
the current revision for new CAS edits. `ExportNew` uses exclusive creation
(`O_EXCL`, mode 0600) and never overwrites an existing path.

## Startup grant plumbing

Trusted run/server startup config stores the grant outside team YAML as
`config.Config.TeamEditMode`, `TeamEditAllowedAgentIDs`, and
`TeamEditAllowedAgentIDsSet`; CLI exposes `--team-edit=off|restricted|full` and
repeatable `--team-edit-agent=<stable-id>`. Empty mode is canonical off.
`nil`/unset allowlist is source-wide only when the trusted mode is enabled; an
explicit empty list with its set bit grants no actor. Remote clients cannot
raise a grant: configure it on server startup. The built-in `team_edit` type is
registered and validated but intentionally exposes zero callable tools until
runtime operation-scoped team adoption is integrated.

CLI/server startup grants are carried in `config.Config`, which is the runtime
portion of snapshot identity. Default-off mode is canonicalized away to preserve
legacy manifest digests; non-default modes and explicit allowlists are included
in round-tripped startup identity. Remote clients must configure a grant on the
server at startup; client flags cannot raise it. An unset client default leaves
the server's grant untouched. `team_edit` is a registered schema/type and
currently validates startup grant but returns no callable tools pending the
runtime adoption handler.

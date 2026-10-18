# Controlled team editing

`team_edit` is a trusted runtime capability, not a tool that YAML or a client can grant itself. Host/CLI startup must grant `--team-edit=restricted` or `--team-edit=full`, and the selected authored agent must declare the toolset:

```yaml
version: "16"
id: incident-team
agents:
  root:
    id: incident-root
    model: openai/gpt-4o
    toolsets:
      - type: team_edit
```

The optional top-level team `id` and per-agent `id` are stable identities. If an ID is omitted, the loader derives identity from the source or authored key. Do not expose credentials in a team ID or agent ID: IDs are persisted with session binding metadata. `--team-edit-agent=<stable-agent-id>` narrows the trusted grant; omitted means source-wide only when mode is enabled. Default mode is `off`. A remote client cannot raise the server's grant; configure authority when starting the server.

Use `team_edit` with `action: "inspect"` and its revision token to inspect the current actor-visible projection. Apply an edit with that revision. `restricted` mode accepts only the supported authored field patches and delegation-graph changes reachable from the actor's captured baseline. `full` mode permits a whole-document `replace`, but the candidate still has to pass ordinary config validation and the trusted host's real team builder before publication. Full replacement is not permission to bypass the host's source graph, loader, permission, provider, budget, or runtime-resource policies. The host must reject any candidate dependency it cannot resolve from trusted captured inputs; a managed/frozen source must not silently load a newly referenced live file or network source.

Every successful update publishes an immutable TeamEnvironment revision. The tool operation stays on its pinned revision; after the edit operation reaches its safe boundary, that session adopts the published revision on its next turn. Other sessions adopt it only at their own safe boundaries. New sessions bind to the current revision. A build, validation, editor/runtime CAS, or source-persistence conflict leaves the published environment unchanged.

Persisting is a separate explicit `team_edit_persist` operation against the current source revision. A direct local YAML source is writable only when the trusted host gives it the exact matching file capability. Managed Kit/workspace sources use the host-provided source-revision compare-and-swap callback; clients never supply its path or revision. URL/frozen sources are never written through a filename preserved for display. `team_edit_export` writes a revision to a new path under the host's trusted export root and never overwrites an existing entry. Tool arguments cannot grant persistence or export authority.

The restricted editor currently accepts a directly captured, latest-version YAML source and rejects includes, flavors, aliases/anchors, ambiguous shapes, `instruction_file`, and shared `use_toolsets`, `use_commands`, or `use_skills` provenance. A full replacement must still be one latest-version YAML document and pass the actual candidate loader/builder. Managed/frozen builders intentionally reject dependencies that were not part of the captured graph; local builders must enforce their own trusted resolver policy. This is not a general live configuration editor for arbitrary external graphs.

Authored instruction text is retained as source content; it may itself contain sensitive literals, so do not put credentials in instructions. Inspection and historical-definition views are partial, display-safe projections: they do not expose provider credentials or resolved environment values. Existing sessions whose stable identity is absent from the current team are transcript-only historical views; they cannot submit turns or invoke team tools.

Only toolsets built for a candidate Team are replaced when that revision publishes. Resources are constructed before publication and failed candidates are discarded. A session continues to hold its previous environment lease until its safe adoption boundary; retirement then stops resources for the old revision after its leases drain.

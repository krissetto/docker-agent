---
title: "Tool and model policy migration"
description: "Migrate custom Go integrations to context-scoped model selection and session-scoped todo storage."
weight: 41
---

## Read-only capability policy

For programmatically constructed agents, use `agent.WithReadOnly(true)`. The
agent filters its static and toolset tools. A custom execution host that adds
implicit tools must apply `ag.FilterTools(finalTools)` **after** those additions,
and use that same filtered list for model exposure and dispatch registration.
Never register removed handlers through a separate fallback registry.

Deferred sources must be filtered before discovery/activation, and code-mode
inner toolsets before wrapping: filtering only the outer JavaScript tool cannot
constrain the functions it exposes. `teamloader` handles these composition
boundaries for configuration-loaded agents. The restriction trusts tool
`ReadOnlyHint` annotations; it is not a filesystem sandbox. Approval and safety
modes are separate and cannot restore a removed capability.

## Context-scoped model selection

`Agent.EffectiveModels` now requires the execution context:

```go
models := ag.EffectiveModels(ctx) // Previously: ag.EffectiveModels()
```

Pass the context supplied to the running turn/tool, not `context.Background()`.
Selection precedence is context-scoped providers, shared runtime overrides, then
configured providers. The returned slice is a copy. `ConfiguredModels()` still
means the manifest defaults, not the active session selection.

For isolated provider selection in custom integrations:

```go
childCtx := agent.WithContextModels(ctx, ag.Name(), providers)
models := ag.EffectiveModels(childCtx)
```

Do not mutate a shared agent and later restore it to emulate a session-local
choice: simultaneous sessions may select different models. Context pins affect
only their named agent and retain pins for other agent identities. The standard
session runtime owns its binding and supplies the appropriate execution context;
do not build another model-binding store alongside it.

CLI model overrides are resolved by the loader. An explicitly configured
`parallel_tool_calls` policy can follow a concrete agent model to a newly created
inline override, without mutating the manifest target model or another agent's
policy. Existing named targets retain their own policies. Omitted policies stay
omitted so provider defaults, rather than synthesized manifest values, apply.

## Custom todo storage (breaking interface change)

Custom `todo.Storage` implementations must implement the context-aware interface:

```go
type Storage interface {
    Add(context.Context, todo.Todo) error
    AllE(context.Context) ([]todo.Todo, error)
    UpdateByID(context.Context, string, func(todo.Todo) todo.Todo) (bool, error)
    Clear(context.Context) error
}
```

Update callers to pass the execution context and handle storage errors. Do not
silently convert a failed load into an empty list or a failed update into success.
`UpdateByID` reports whether the ID existed independently of its error result.

Use `todo.NewSessionStorage(store, resolveSessionID)` when integrating an existing
session store. Its `SessionStore` backend supplies `LoadTodos`, `SaveTodos`, and
atomic `MutateTodos` operations keyed by session ID. Resolve the ID from the
execution context so children and concurrent sessions do not share one global
list. The mutation callback must run within the store's atomic operation; a
separate load/modify/save sequence loses concurrent updates. An empty resolved ID
uses local in-memory fallback and does not promise durable session storage.

Do not persist todos by writing a cached full-session snapshot back over the
session owner's live state. Keep one authoritative session store and expose its
atomic todo operations. For nonpersistent, standalone tools,
`todo.NewMemoryTodoStorage()` remains available.

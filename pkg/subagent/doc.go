// Package subagent holds the shared vocabulary of the async subagent feature:
// the node/tree model of a running swarm, the model-facing core tools, and the
// persistence contract. The runtime machinery that drives subagents lives in
// pkg/runtime (its subagent manager); this package deliberately has no
// dependency on the concrete agent loop.
//
//   - [Node], [Snapshot] and [Tree] describe a swarm of running agent
//     instances. Tree is the thread-safe live registry; it drives UIs via
//     [Tree.Subscribe] and serialises to [Snapshot] for persistence.
//   - [Definitions] are the core tools (spawn_subagent, send_message,
//     read_subagent, stop_subagent) and [HarnessPrompt] builds the core system
//     prompt guidance plus allow-list for agents declaring `subagents:`. The
//     [ToolSet] only supplies tool definitions; harness guidance is not a
//     toolset instruction. The set is intentionally tiny — there is no end_turn
//     tool: an agent ends its turn by finishing its response, and status
//     updates from its subagents wake it later.
//   - [WrapSystemInfo]/[IsSystemInfo] frame the notes the runtime writes on
//     the agents' behalf (spawn tasks, turn reports, relayed messages) so the
//     model (and transcript renderers) can tell harness notifications from
//     ordinary conversation.
//   - [Store] is the snapshot persistence contract. The built-in SQLite
//     session store implements it; embedders can supply their own via
//     runtime.WithSubagentStore.
package subagent

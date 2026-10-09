---
title: "MCP Mode"
description: "Expose your Docker Agent agents as MCP tools for use in Claude Desktop, Claude Code, and other MCP-compatible applications."
keywords: docker agent, ai agents, features, mcp mode
weight: 40
canonical: https://docs.docker.com/ai/docker-agent/features/mcp-mode/
---

_Expose your Docker Agent agents as MCP tools for use in Claude Desktop, Claude Code, and other MCP-compatible applications._

## Why MCP Mode?

The `docker agent serve mcp` command makes your agents available to any application that supports the [Model Context Protocol](https://modelcontextprotocol.io/). This means you can:

- Use custom agents directly within **Claude Desktop** or **Claude Code**
- Share specialized agents across different applications
- Build reusable agent teams consumable from any MCP client
- Integrate domain-specific agents into existing workflows

> [!NOTE]
> **What is MCP?**
>
> The [Model Context Protocol](https://modelcontextprotocol.io/) is an open standard for connecting AI tools. See also [Remote MCP Servers](../remote-mcp/index.md) for connecting to cloud services.

## Basic Usage

```bash
# Expose a local config (stdio transport, the default)
$ docker agent serve mcp ./agent.yaml

# Expose from a registry
$ docker agent serve mcp myorg/agent:tag

# Set the working directory
$ docker agent serve mcp ./agent.yaml --working-dir /path/to/project
```

## Transports

By default, `serve mcp` uses the stdio transport — ideal for clients that spawn the server as a subprocess (Claude Desktop, Claude Code, Cursor, …).

To expose the MCP server over streaming HTTP instead, pass `--http`:

```bash
# Streaming HTTP transport on the default 127.0.0.1:8081
$ docker agent serve mcp ./agent.yaml --http

# Override the listen address / port; non-loopback HTTP requires authentication
$ docker agent serve mcp ./agent.yaml --http --listen 0.0.0.0:9090 --auth-token "$MCP_BEARER_TOKEN"
```

| Flag                   | Default            | Description                                                                                                  |
| ---------------------- | ------------------ | ------------------------------------------------------------------------------------------------------------ |
| `--http`               | `false`            | Use streaming HTTP transport instead of stdio.                                                               |
| `-l`, `--listen`       | `127.0.0.1:8081`   | Address to listen on when `--http` is enabled.                                                               |
| `-a`, `--agent`        | all agents         | Expose a single named agent instead of every agent in the config.                                            |
| `--tool-name`          | (none)             | Override the MCP tool identifier clients call (defaults to agent name); only valid when exposing one agent.  |
| `--auth-token`          | (none)             | Require this Bearer token for HTTP requests. Required for non-loopback HTTP unless explicitly overridden.       |
| `--insecure-no-auth`    | `false`            | Permit unauthenticated non-loopback HTTP. Use only behind a trusted authentication boundary.                    |
| `--safety`              | `restricted`       | Tool safety policy for HTTP requests. CLI value overrides agent/runtime configuration.                           |
| `--mcp-keepalive`      | `0`                | Interval between MCP keep-alive pings (e.g. `30s`); `0` disables keep-alive. Only when serving an agent over stdio — rejected with `--http` and `--attach`.  |

Runtime configuration flags such as `--working-dir`, `--env-from-file`, `--models-gateway`, and hook flags are also available — see the [CLI reference](../cli/index.md).

The HTTP transport is **stateless**, per MCP spec revision `2026-07-28`: modern clients negotiate via `server/discover`, no `Mcp-Session-Id` is issued, and only POST requests are served (GET and DELETE answer `405 Method Not Allowed`). Clients speaking older protocol revisions keep working — the legacy `initialize` handshake is accepted with per-request state. However, older stateful clients that depend on a standalone GET stream or session `DELETE` teardown must upgrade to (or switch to) a client compatible with stateless streaming HTTP. Because the stateless transport has no server-initiated ping, `--mcp-keepalive` is rejected together with `--http`; keep-alive remains available on the stdio transport.

## HTTP security

HTTP MCP defaults to loopback binding. A non-loopback `--listen` address requires `--auth-token`; use `--insecure-no-auth` only when a trusted reverse proxy or network boundary authenticates clients. The safety policy is resolved in this order: `--safety`, agent configuration, runtime configuration, then `restricted`. These HTTP-only flags do not affect stdio or `--attach` operation.

## Using with Claude Desktop

Add a configuration to your Claude Desktop MCP settings file:

- **macOS:** `~/Library/Application Support/Claude/claude_desktop_config.json`
- **Windows:** `%APPDATA%\Claude\claude_desktop_config.json`

```json
{
  "mcpServers": {
    "myagent": {
      "command": "/usr/local/bin/docker",
      "args": [
        "agent", 
        "serve",
        "mcp",
        "myorg/coder",
        "--working-dir",
        "/home/user/projects"
      ],
      "env": {
        "ANTHROPIC_API_KEY": "your_key_here",
        "OPENAI_API_KEY": "your_key_here"
      }
    }
  }
}
```

Restart Claude Desktop after updating the configuration.

## Using with Claude Code

```bash
$ claude mcp add --transport stdio myagent \
  --env OPENAI_API_KEY=$OPENAI_API_KEY \
  --env ANTHROPIC_API_KEY=$ANTHROPIC_API_KEY \
  -- docker agent serve mcp myorg/agent:tag --working-dir $(pwd)
```

## Multi-Agent in MCP Mode

When you expose a multi-agent configuration via MCP, each agent becomes a separate tool in the MCP client:

```yaml
agents:
  root:
    model: anthropic/claude-sonnet-4-5
    description: Main coordinator
    sub_agents: [designer, engineer]
  designer:
    model: openai/gpt-5-mini
    description: UI/UX design specialist
  engineer:
    model: anthropic/claude-sonnet-4-5
    description: Software engineer
```

All three agents (`root`, `designer`, `engineer`) appear as separate tools in Claude Desktop or Claude Code.

## Troubleshooting

- **Agents not appearing:** Verify the `docker-agent` binary path and restart the MCP client
- **Permission errors:** Ensure `docker-agent` has execute permissions (`chmod +x`)
- **Missing API keys:** Pass all required keys in the `env` section
- **Working directory issues:** Verify the `--working-dir` path exists and is accessible

## Attach to an existing canonical session

Attachment borrows execution from a running TUI control plane or authenticated
`serve api` authority. It does not create another runtime or change approval
policy. For a trusted local run, use `docker agent serve mcp --attach` (latest),
or `--attach <pid|address|session-id>`.

For an explicit authority and canonical session ID:

```console
$ docker agent serve mcp --attach-address https://agents.example.com --attach-session SESSION_ID --attach-token-env DOCKER_AGENT_ATTACH_TOKEN
```

Provision `DOCKER_AGENT_ATTACH_TOKEN` through your MCP client's secret environment
configuration. Do not put tokens in command arguments or URLs. URLs containing
credentials, queries or fragments are rejected; non-loopback authorities require
HTTPS and a token. This is a server-wide bearer authorization boundary, not a
per-user/per-session ACL. Only attach to an authority you trust with session data.
Child sessions additionally require `--attach-confirm-child`; attachment uses the
canonical confirmed-view route rather than bypassing child admission.

The attachment exposes these tools:

| Tool | Behavior |
| --- | --- |
| `send` | Asynchronous steer, or FIFO submission with `followup: true`. Returns canonical `session_id`, accepted `turn_id`, and `disposition`; optional `request_id` makes admission retryable. |
| `read` | Current canonical status, epoch/cursor, supported controls and outstanding interaction IDs/kinds/payloads, including elicitation form schemas. |
| `transcript` | Read recent text messages (`limit: 0` means all). |
| `await_turn` | Wait for settlement of exactly `turn_id`. Cancelling the wait only detaches. |
| `cancel_turn` | Cancel exactly `turn_id`, never a newer or unrelated turn. Use `await_turn` to join settlement. |
| `respond` | Answer exactly one discovered interaction; stale or foreign tokens fail. |
| `stop_subtree` | Advertised only when supported. Fence and drain the attached subtree, retaining history. |
| `delegation_policy` | Advertised only when supported. Omit `enabled` to read; supply a boolean to update under the authority's root policy rules. |

For example, a form response discovered by `read` is:

```json
{"interaction_id":"REQUEST_ID","kind":"elicitation","elicitation_id":"ELICITATION_ID","action":"accept","content":{"answer":"yes"}}
```

Use `action: "decline"` or `"cancel"` to decline/cancel elicitation. For confirmation
or max-iteration requests use the discovered kind (`confirmation` or
`max_iterations`) with `confirmation: "approve"` or `"reject"`. Confirmation also
accepts canonical `approve-balanced`, `approve-autonomous`, and `approve-tool`
(with `tool_name`) decisions; these explicitly change policy and are never chosen
automatically. Form content is validated by the canonical authority.

`send` intentionally does not synchronously own an execution lifetime: accepted
input survives completion/cancellation of the MCP request. Cancellation before
acknowledgement can leave admission uncertain; retry with the same `request_id`
and unchanged input to reconcile it, rather than sending a new identity. Closing
the MCP connection only detaches; it never cancels, deletes, stops, or shuts down
the borrowed session. Unsupported optional controls are not listed and cannot
be invoked.

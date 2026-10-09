package root

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/mcp"
	"github.com/docker/docker-agent/pkg/runregistry"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/servesafety"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/telemetry"
)

type mcpFlags struct {
	agentName          string
	http               bool
	listenAddr         string
	attach             string
	attachAddress      string
	attachSession      string
	attachTokenEnv     string
	attachConfirmChild bool
	safety             string
	authToken          string
	insecureNoAuth     bool
	runConfig          config.RuntimeConfig
}

func newMCPCmd() *cobra.Command {
	var flags mcpFlags

	cmd := &cobra.Command{
		Use:   "mcp <agent-file>|<registry-ref>",
		Short: "Start an agent as an MCP (Model Context Protocol) server",
		Long:  "Start an MCP server that exposes the agent via the Model Context Protocol. By default, uses stdio transport. Use --http to start a streaming HTTP server instead. Use --attach to expose a running TUI's session, or --attach-address and --attach-session for an authenticated canonical authority, instead of an agent file.",
		Example: `  docker-agent serve mcp ./agent.yaml
  docker-agent serve mcp ./team.yaml
  docker-agent serve mcp myorg/agent:tag
  docker-agent serve mcp ./agent.yaml --http --listen 127.0.0.1:9090
  docker-agent serve mcp --attach
  docker-agent serve mcp --attach-address https://agents.example.com --attach-session SESSION_ID`,
		Args: cobra.MaximumNArgs(1),
		RunE: flags.runMCPCommand,
	}

	cmd.PersistentFlags().StringVarP(&flags.agentName, "agent", "a", "", "Name of the agent to run (all agents if not specified)")
	cmd.PersistentFlags().BoolVar(&flags.http, "http", false, "Use streaming HTTP transport instead of stdio")
	cmd.PersistentFlags().StringVarP(&flags.listenAddr, "listen", "l", "127.0.0.1:8081", "Address to listen on")
	cmd.PersistentFlags().StringVar(&flags.attach, "attach", "", "Attach to a running TUI run by pid, address, or session id (or empty for the most recent)")
	cmd.PersistentFlags().Lookup("attach").NoOptDefVal = "latest"
	cmd.PersistentFlags().StringVar(&flags.attachAddress, "attach-address", "", "Explicit canonical authority URL; requires --attach-session")
	cmd.PersistentFlags().StringVar(&flags.attachSession, "attach-session", "", "Canonical session ID at --attach-address")
	cmd.PersistentFlags().StringVar(&flags.attachTokenEnv, "attach-token-env", "DOCKER_AGENT_ATTACH_TOKEN", "Environment variable containing the authority bearer token (never put credentials in URLs)")
	cmd.PersistentFlags().BoolVar(&flags.attachConfirmChild, "attach-confirm-child", false, "Confirm canonical child session attachment")
	cmd.PersistentFlags().StringVar(&flags.safety, "safety", "", "Tool safety policy (strict, balanced, restricted, autonomous); only valid with --http")
	cmd.PersistentFlags().StringVar(&flags.authToken, "auth-token", "", "Bearer token required for HTTP MCP requests; only valid with --http")
	cmd.PersistentFlags().BoolVar(&flags.insecureNoAuth, "insecure-no-auth", false, "Allow unauthenticated non-loopback HTTP binding (insecure); only valid with --http")
	cmd.PersistentFlags().StringVar(&flags.runConfig.MCPToolName, "tool-name", "", "Override the MCP tool identifier clients call (defaults to agent name); only valid when exposing a single agent")
	cmd.PersistentFlags().DurationVar(&flags.runConfig.MCPKeepAlive, "mcp-keepalive", 0, "Interval between MCP keep-alive pings (e.g. 30s); 0 disables keep-alive; only valid when serving an agent over stdio (not --http or --attach)")
	addRuntimeConfigFlags(cmd, &flags.runConfig)

	return cmd
}

func (f *mcpFlags) runMCPCommand(cmd *cobra.Command, args []string) (commandErr error) {
	ctx := cmd.Context()
	telemetry.TrackCommand(ctx, "serve", append([]string{"mcp"}, args...))
	defer func() { // do not inline this defer so that commandErr is not resolved early
		telemetry.TrackCommandError(ctx, "serve", append([]string{"mcp"}, args...), commandErr)
	}()

	if f.attach != "" || f.attachAddress != "" || f.attachSession != "" {
		if len(args) != 0 {
			return errors.New("agent file cannot be used with attachment")
		}
		if f.http || f.safety != "" || f.authToken != "" || f.insecureNoAuth {
			return errors.New("--http-only safety and authentication flags cannot be used with --attach")
		}
		if f.runConfig.MCPKeepAlive != 0 {
			return errors.New("--mcp-keepalive cannot be used with --attach: the attach proxy ignores runtime configuration")
		}
		return f.runAttach(ctx)
	}

	if !f.http && (f.safety != "" || f.authToken != "" || f.insecureNoAuth) {
		return errors.New("--safety, --auth-token, and --insecure-no-auth require --http")
	}
	if f.http && f.runConfig.MCPKeepAlive != 0 {
		return errors.New("--mcp-keepalive is not supported with --http: stateless HTTP MCP does not support server-initiated keep-alive pings; use the stdio transport instead")
	}
	if err := validateSafetyFlag(f.safety); err != nil {
		return err
	}

	if len(args) == 0 {
		return errors.New("agent file is required (or use --attach)")
	}
	agentFilename := args[0]

	if !f.http {
		return mcp.StartMCPServer(ctx, agentFilename, f.agentName, &f.runConfig)
	}

	if !isLoopbackListenAddr(f.listenAddr) && f.authToken == "" && !f.insecureNoAuth {
		return errors.New("non-loopback MCP HTTP listeners require --auth-token or --insecure-no-auth")
	}

	ln, cleanup, err := newListener(ctx, f.listenAddr)
	if err != nil {
		return err
	}
	defer cleanup()

	return mcp.StartHTTPServer(ctx, agentFilename, f.agentName, &f.runConfig, ln, mcp.HTTPOptions{
		CLISafety: session.SafetyPolicy(f.safety),
		AuthToken: f.authToken,
		OnSafetyPolicy: func(resolved servesafety.Resolved) {
			fmt.Fprintf(cmd.OutOrStdout(), "Tool safety policy: %s (source: %s)\n", resolved.Policy, resolved.Source)
		},
	})
}

func (f *mcpFlags) runAttach(ctx context.Context) error {
	addr, id := f.attachAddress, f.attachSession
	if addr != "" || id != "" {
		if addr == "" || id == "" {
			return errors.New("--attach-address and --attach-session must be used together")
		}
		if f.attach != "" && f.attach != "latest" {
			return errors.New("explicit authority cannot be combined with a local --attach target")
		}
	} else {
		target := f.attach
		if target == "latest" {
			target = ""
		}
		rec, err := runregistry.Default().Find(target)
		if err != nil {
			return err
		}
		addr, id = rec.Addr, rec.SessionID
	}
	token := os.Getenv(f.attachTokenEnv)
	if err := validateAttachAddress(addr, token); err != nil {
		return err
	}
	return mcp.StartAttachStdioWithOptions(ctx, addr, id, mcp.AttachOptions{ClientOptions: []runtime.ClientOption{runtime.WithAuthToken(token)}, ConfirmChild: f.attachConfirmChild})
}

func validateAttachAddress(addr, token string) error {
	u, err := url.Parse(addr)
	if err != nil || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return errors.New("attach authority must be an HTTP(S) URL without credentials, query or fragment")
	}
	host := u.Hostname()
	ip := net.ParseIP(host)
	loopback := strings.EqualFold(host, "localhost") || (ip != nil && ip.IsLoopback())
	if !loopback && (u.Scheme != "https" || token == "") {
		return errors.New("non-loopback attachment requires HTTPS and a bearer token supplied through --attach-token-env")
	}
	return nil
}

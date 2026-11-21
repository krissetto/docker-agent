package root

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/docker/docker-agent/pkg/acp"
	"github.com/docker/docker-agent/pkg/config"
	pathx "github.com/docker/docker-agent/pkg/path"
	"github.com/docker/docker-agent/pkg/telemetry"
)

type acpFlags struct {
	runConfig          config.RuntimeConfig
	sessionDB          string
	managedAPI         bool
	managedAPIAttach   bool
	managedAPIStateDir string
}

func newACPCmd() *cobra.Command {
	var flags acpFlags

	cmd := &cobra.Command{
		Use:   "acp <agent-file>|<registry-ref>",
		Short: "Start an agent as an ACP (Agent Client Protocol) server",
		Long:  "Start an ACP server that exposes the agent via the Agent Client Protocol",
		Example: `  docker-agent serve acp ./agent.yaml
  docker-agent serve acp ./team.yaml
  docker-agent serve acp myorg/agent:tag`,
		Args: cobra.ExactArgs(1),
		RunE: flags.runACPCommand,
	}

	cmd.Flags().StringVarP(&flags.sessionDB, "session-db", "s", "", "Path to the session database (default: <data-dir>/session.db)")
	addRuntimeConfigFlags(cmd, &flags.runConfig)
	cmd.Flags().BoolVar(&flags.managedAPI, "managed-api", false, "Use an authenticated managed API for shared ACP sessions, execution and persistence")
	cmd.Flags().BoolVar(&flags.managedAPIAttach, "managed-api-attach", false, "Attach to a lifecycle-owned managed API without starting it")
	cmd.Flags().StringVar(&flags.managedAPIStateDir, "managed-api-state-dir", "", "Private managed API state directory")
	cmd.MarkFlagsMutuallyExclusive("managed-api", "session-db")

	return cmd
}

func (f *acpFlags) runACPCommand(cmd *cobra.Command, args []string) (commandErr error) {
	ctx := cmd.Context()
	telemetry.TrackCommand(ctx, "serve", append([]string{"acp"}, args...))
	defer func() { // do not inline this defer so that commandErr is not resolved early
		telemetry.TrackCommandError(ctx, "serve", append([]string{"acp"}, args...), commandErr)
	}()

	agentFilename := args[0]
	if !f.managedAPI && (f.managedAPIAttach || f.managedAPIStateDir != "") {
		return errors.New("--managed-api-attach and --managed-api-state-dir require --managed-api")
	}
	if f.managedAPI {
		if f.managedAPIAttach {
			// These flags configure local execution, not the committed API. Silently
			// accepting them would imply authority this stdio connection does not own.
			for _, name := range []string{"env-from-file", "flavor", "team-edit", "team-edit-agent", "code-mode-tools", "models-gateway", "hook-pre-tool-use", "hook-post-tool-use", "hook-session-start", "hook-session-end", "hook-on-user-input", "hook-stop", "mcp-oauth-redirect-uri"} {
				if cmd.Flags().Changed(name) {
					return fmt.Errorf("--%s cannot configure a lifecycle-owned managed ACP API; configure it at startup", name)
				}
			}
		}
		// Bootstrap/attach uses exactly the run command's authenticated lifecycle,
		// immutable team snapshot and workspace authority. It never opens SQLite here.
		managed := &runExecFlags{managedAPI: true, managedAPIAttach: f.managedAPIAttach, managedAPIStateDir: f.managedAPIStateDir, sessionDB: f.sessionDB}
		managed.runConfig.Config = f.runConfig.Config
		source, err := managed.bootstrapManagedAPI(ctx, agentFilename)
		if err != nil {
			return err
		}
		if f.managedAPIAttach && source != agentFilename {
			return errors.New("managed ACP team must match the lifecycle-owned API source; recreate the sandbox to change teams")
		}
		client, err := newRemoteClient(managed.remoteAddress, managed.remoteAuthTokenFile)
		if err != nil {
			return err
		}
		return acp.RunManaged(ctx, client, source, managed.remoteWorkingDir, cmd.InOrStdin(), cmd.OutOrStdout())
	}

	// Expand tilde in session database path
	sessionDB, err := pathx.ExpandHomeDir(sessionDBPath(f.sessionDB))
	if err != nil {
		return err
	}

	return acp.Run(ctx, agentFilename, cmd.InOrStdin(), cmd.OutOrStdout(), &f.runConfig, sessionDB)
}

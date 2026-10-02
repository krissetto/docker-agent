package root

import (
	"context"
	"errors"
	"fmt"
	"github.com/docker/docker-agent/pkg/runtime"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	pathx "github.com/docker/docker-agent/pkg/path"
	"github.com/docker/docker-agent/pkg/session/sqlitestore"
)

func newSessionsListCmd() *cobra.Command {
	var sessionDB string
	var quiet bool
	var managed bool
	var stateDir, workingDir, remote, tokenFile string
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List resumable sessions, newest first",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var ids []string
			var err error
			switch {
			case managed:
				ids, err = managedSessionIDs(cmd.Context(), stateDir, workingDir)
			case remote != "":
				ids, err = remoteSessionIDs(cmd.Context(), remote, tokenFile, "", "")
			default:
				var path string
				path, err = pathx.ExpandHomeDir(sessionDBPath(sessionDB))
				if err == nil {
					ids, err = sqlitestore.ListSessionIDs(cmd.Context(), path)
				}
			}
			if err != nil {
				return fmt.Errorf("listing sessions: %w", err)
			}
			if !quiet {
				if _, err := fmt.Fprintln(cmd.OutOrStdout(), "SESSION ID"); err != nil {
					return err
				}
			}
			for _, id := range ids {
				if _, err := fmt.Fprintln(cmd.OutOrStdout(), id); err != nil {
					return err
				}
			}
			return nil
		},
	}
	cmd.Flags().StringVarP(&sessionDB, "session-db", "s", "", "Path to the session database (default: <data-dir>/session.db)")
	cmd.Flags().BoolVarP(&quiet, "quiet", "q", false, "Print only full session IDs, one per line")
	cmd.Flags().BoolVar(&managed, "managed-api", false, "List sessions from the workspace managed API")
	cmd.Flags().StringVar(&stateDir, "managed-api-state-dir", "", "Private managed API state directory")
	cmd.Flags().StringVar(&workingDir, "working-dir", "", "Managed API workspace (default: current directory)")
	cmd.Flags().StringVar(&remote, "remote", "", "List sessions from this API address")
	cmd.Flags().StringVar(&tokenFile, "remote-auth-token-file", "", "Private API bearer token file")
	cmd.MarkFlagsMutuallyExclusive("managed-api", "remote", "session-db")
	return cmd
}

func isSessionsListCommand(cmd *cobra.Command) bool {
	return cmd != nil && cmd.Name() == "list" && cmd.Parent() != nil && cmd.Parent().Name() == "sessions"
}

// Catalog queries never instantiate a runtime or open a second SQLite store.
func managedSessionIDs(ctx context.Context, base, root string) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, managedStartupTimeout)
	defer cancel()
	workspace, err := managedWorkspace(root)
	if err != nil {
		return nil, err
	}
	dir, err := managedStateDir(base, workspace)
	if err != nil {
		return nil, err
	}
	d, err := ensureManagedAPI(ctx, dir, nil)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return remoteSessionIDs(ctx, d.Address, filepath.Join(dir, "token"), d.Source, workspace)
}

func remoteSessionIDs(ctx context.Context, address, tokenFile, source, workspace string) ([]string, error) {
	client, err := newRemoteClient(address, tokenFile)
	if err != nil {
		return nil, err
	}
	transport, err := runtime.NewSessionTransport(client)
	if err != nil {
		return nil, err
	}
	rows, err := transport.ListSessionSummaries(ctx, runtime.SessionSummaryOptions{})
	if err != nil {
		return nil, err
	}
	rows = slices.DeleteFunc(rows, func(row runtime.SessionSummaryEntry) bool {
		return row.ParentID != "" || (source != "" && row.Source != source) || (workspace != "" && row.WorkingDir != workspace)
	})
	slices.SortStableFunc(rows, func(a, b runtime.SessionSummaryEntry) int {
		if n := b.CreatedAt.Compare(a.CreatedAt); n != 0 {
			return n
		}
		return strings.Compare(a.SessionID, b.SessionID)
	})
	ids := make([]string, len(rows))
	for i, row := range rows {
		ids[i] = row.SessionID
	}
	return ids, nil
}

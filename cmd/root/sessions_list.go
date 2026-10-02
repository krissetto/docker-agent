package root

import (
	"fmt"

	"github.com/spf13/cobra"

	pathx "github.com/docker/docker-agent/pkg/path"
	"github.com/docker/docker-agent/pkg/session/sqlitestore"
)

func newSessionsListCmd() *cobra.Command {
	var sessionDB string
	var quiet bool
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List resumable sessions, newest first",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			path, err := pathx.ExpandHomeDir(sessionDBPath(sessionDB))
			if err != nil {
				return err
			}
			ids, err := sqlitestore.ListSessionIDs(cmd.Context(), path)
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
	return cmd
}

func isSessionsListCommand(cmd *cobra.Command) bool {
	return cmd != nil && cmd.Name() == "list" && cmd.Parent() != nil && cmd.Parent().Name() == "sessions"
}

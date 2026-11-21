package root

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestACPManagedModeRejectsIgnoredConfiguration(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"attach requires mode", []string{"--managed-api-attach"}, "require --managed-api"},
		{"state requires mode", []string{"--managed-api-state-dir", t.TempDir()}, "require --managed-api"},
		{"team authority", []string{"--managed-api", "--managed-api-attach", "--team-edit=full"}, "cannot configure a lifecycle-owned"},
		{"flavor authority", []string{"--managed-api", "--managed-api-attach", "--flavor=other"}, "cannot configure a lifecycle-owned"},
		{"hook authority", []string{"--managed-api", "--managed-api-attach", "--hook-on-user-input=ignored"}, "cannot configure a lifecycle-owned"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := newACPCmd()
			require.NoError(t, cmd.ParseFlags(tc.args))
			// Exercise command dispatch without root's user-config/environment setup.
			err := cmd.RunE(cmd, []string{"team.yaml"})
			require.ErrorContains(t, err, tc.want)
		})
	}
	cmd := newACPCmd()
	require.NoError(t, cmd.ParseFlags([]string{"--managed-api", "--session-db=other.db"}))
	require.Error(t, cmd.ValidateFlagGroups())
}

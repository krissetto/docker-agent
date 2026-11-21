package root

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRunInSandboxRejectsSandboxMarkersBeforeBackendAccess(t *testing.T) {
	for _, tc := range []struct {
		name, marker, vmID, want string
	}{
		{name: "modern", marker: "1", want: "already running inside a Docker sandbox"},
		{name: "legacy", vmID: "synthetic-vm", want: "already running inside a Docker sandbox (VM synthetic-vm)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("IS_SANDBOX", tc.marker)
			t.Setenv("SANDBOX_VM_ID", tc.vmID)
			t.Setenv("SANDBOX_ID", "opaque-runtime-id")
			err := runInSandbox(t.Context(), nil, nil, nil, "", false, false, nil)
			require.EqualError(t, err, tc.want)
		})
	}
}

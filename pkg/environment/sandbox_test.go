package environment

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestInSandbox(t *testing.T) {
	for _, tc := range []struct {
		name, marker, vmID string
		want               bool
	}{
		{name: "outside sandbox"},
		{name: "modern sandbox", marker: "1", want: true},
		{name: "legacy sandbox", vmID: "some-uuid", want: true},
		{name: "both markers", marker: "1", vmID: "some-uuid", want: true},
		{name: "zero marker", marker: "0"},
		{name: "nonstandard true marker", marker: "true"},
		{name: "padded marker", marker: " 1 "},
		{name: "legacy fallback", marker: "0", vmID: "some-uuid", want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("IS_SANDBOX", tc.marker)
			t.Setenv("SANDBOX_VM_ID", tc.vmID)
			assert.Equal(t, tc.want, InSandbox())
		})
	}
}

func TestSandboxIdentityAloneDoesNotImplySandbox(t *testing.T) {
	t.Setenv("IS_SANDBOX", "")
	t.Setenv("SANDBOX_VM_ID", "")
	t.Setenv("SANDBOX_NAME", "a-name")
	t.Setenv("SANDBOX_ID", "opaque-id")
	assert.False(t, InSandbox())
}

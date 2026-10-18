//go:build !windows && !js

package environment

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOnePasswordProvider_DoesNotInvokeCLIUnderGoTest(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "invoked")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "op"), []byte("#!/bin/sh\ntouch \"$OP_TEST_MARKER\"\nprintf 'fake-secret\\n'\n"), 0o700))
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("OP_TEST_MARKER", marker)

	provider := NewOnePasswordProvider(NewMapEnvProvider(map[string]string{
		"API_KEY": "op://test/item/field",
	}))
	value, found := provider.Get(t.Context(), "API_KEY")
	assert.True(t, found)
	assert.Empty(t, value)
	assert.NoFileExists(t, marker)
}

func TestReadOnePasswordReference_FakeCLI(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		script string
		found  bool
		value  string
	}{
		{
			name: "success",
			script: `test "$#" = 2 && test "$1" = read && test "$2" = op://test/item/field || exit 2
printf 'diagnostic only\n' >&2
printf '  fake-secret  \n'`,
			found: true,
			value: "fake-secret",
		},
		{
			name: "failure discards output",
			script: `printf 'fake-secret\n'
printf 'No accounts configured for use with 1Password CLI\n' >&2
exit 1`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "op")
			require.NoError(t, os.WriteFile(path, []byte("#!/bin/sh\n"+tc.script+"\n"), 0o700))
			value, found := readOnePasswordReference(t.Context(), path, "op://test/item/field")
			assert.Equal(t, tc.found, found)
			assert.Equal(t, tc.value, value)
		})
	}
}

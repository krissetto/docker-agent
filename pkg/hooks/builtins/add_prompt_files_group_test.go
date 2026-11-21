package builtins

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/docker/docker-agent/pkg/hooks"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAddPromptFilesIndependentGroups(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("DOCKER_AGENT_KIT_DIR", "")
	path := filepath.Join(home, "AGENTS.md")
	require.NoError(t, os.WriteFile(path, []byte("User instructions."), 0o600))
	input := &hooks.Input{Cwd: home}
	ordinary, err := addPromptFiles(t.Context(), input, []string{"AGENTS.md"})
	require.NoError(t, err)
	independent, err := addPromptFiles(t.Context(), input, []string{"--group=kit/context", "AGENTS.md"})
	require.NoError(t, err)
	a := ordinary.HookSpecificOutput.InstructionContext[0]
	b := independent.HookSpecificOutput.InstructionContext[0]
	assert.Equal(t, promptFilesGroup, a.Group)
	assert.Equal(t, "kit/context", b.Group)
	assert.NotEqual(t, a.Key, b.Key)
	assert.True(t, a.CompleteGroup)
	assert.True(t, b.CompleteGroup)
	require.NoError(t, os.Remove(path))
	missing, err := addPromptFiles(t.Context(), input, []string{"--group=kit/context", "kit-absent.md"})
	require.NoError(t, err)
	marker := missing.HookSpecificOutput.InstructionContext[0]
	assert.Equal(t, "kit/context", marker.Group)
	assert.True(t, marker.CompleteGroup)
	assert.True(t, marker.SetMarker)
	for _, args := range [][]string{{"--group", "AGENTS.md"}, {"--group=", "AGENTS.md"}, {"--group= ", "AGENTS.md"}, {"--group=kit/context"}} {
		_, err := addPromptFiles(t.Context(), input, args)
		require.Error(t, err, args)
	}
}

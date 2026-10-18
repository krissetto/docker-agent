//go:build !windows && !js

package environment

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/goccy/go-yaml"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTaskTestCommands_ClearInheritedAPIKeys(t *testing.T) {
	repository, err := filepath.Abs("../..")
	require.NoError(t, err)
	raw, err := os.ReadFile(filepath.Join(repository, "Taskfile.yml"))
	require.NoError(t, err)
	var taskfile struct {
		Tasks struct {
			Test struct {
				Cmds []string `yaml:"cmds"`
			} `yaml:"test"`
			Race struct {
				Cmds []string `yaml:"cmds"`
			} `yaml:"test-race"`
			Binary struct {
				Cmds []string `yaml:"cmds"`
			} `yaml:"test-binary"`
		} `yaml:"tasks"`
	}
	require.NoError(t, yaml.Unmarshal(raw, &taskfile))

	// Substitute only Go: neither the full suite nor any credential CLI runs.
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "go"), []byte(`#!/bin/sh
for name in DOCKER_AGENT_MODELS_GATEWAY OPENAI_API_KEY ANTHROPIC_API_KEY GOOGLE_API_KEY GOOGLE_GENAI_USE_VERTEXAI MISTRAL_API_KEY OPENROUTER_API_KEY; do
  eval 'value=${'"$name"'-unset}'
  test "$value" = '' || { echo "inherited value for $name" >&2; exit 1; }
done
printf '%s\n' "$*"
`), 0o700))

	for _, tc := range []struct {
		task, args string
		cmds       []string
	}{
		{"test", "test ./...\n", taskfile.Tasks.Test.Cmds},
		{"test-race", "test -race -shuffle=on ./...\n", taskfile.Tasks.Race.Cmds},
		{"test-binary", "test -tags=binary_required ./e2e/binary/...\n", taskfile.Tasks.Binary.Cmds},
	} {
		t.Run(tc.task, func(t *testing.T) {
			t.Parallel()
			require.Len(t, tc.cmds, 1)
			cmd := exec.CommandContext(t.Context(), "/bin/sh", "-c", tc.cmds[0])
			cmd.Dir = repository
			cmd.Env = append(os.Environ(), "PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"))
			for _, name := range []string{"DOCKER_AGENT_MODELS_GATEWAY", "OPENAI_API_KEY", "ANTHROPIC_API_KEY", "GOOGLE_API_KEY", "GOOGLE_GENAI_USE_VERTEXAI", "MISTRAL_API_KEY", "OPENROUTER_API_KEY"} {
				cmd.Env = append(cmd.Env, name+"=op://test/item/field")
			}
			output, err := cmd.CombinedOutput()
			require.NoError(t, err, "%s", output)
			assert.Equal(t, tc.args, string(output))
		})
	}
}

package kit_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Resolve checkout assets before a test changes into its isolated workspace.
// Go runs this package from tests/kit, regardless of the caller's directory.
func repositoryPath(t *testing.T, parts ...string) string {
	t.Helper()
	path, err := filepath.Abs(filepath.Join(append([]string{"..", ".."}, parts...)...))
	require.NoError(t, err)
	return path
}

func kitPath(t *testing.T, name string) string {
	t.Helper()
	return repositoryPath(t, "kit", name)
}

func TestKitBuildInputIsolation(t *testing.T) {
	raw, err := os.ReadFile(kitPath(t, "async-agent.dockerfile.dockerignore"))
	require.NoError(t, err)
	var rules []string
	for line := range strings.SplitSeq(string(raw), "\n") {
		if line = strings.TrimSpace(line); line != "" && !strings.HasPrefix(line, "#") {
			rules = append(rules, line)
		}
	}
	// Deny by default, then allow only compilation inputs and runtime assets.
	// An explicit allowlist guards against accidentally including credentials,
	// databases, docs, test fixtures or unrelated untracked workspace files.
	assert.Equal(t, []string{
		"**",
		"!go.mod", "!go.sum", "!main.go", "!cmd/**/*.go", "!pkg/**/*.go",
		"!pkg/app/export/export.css", "!pkg/app/export/export.html", "!pkg/app/export/export.js",
		"!pkg/chatserver/openapi.json",
		"!pkg/compaction/prompts/compaction-system.txt", "!pkg/compaction/prompts/compaction-user.txt",
		"!pkg/config/sources/builtin-agents/*.yaml", "!pkg/creator/instructions.txt",
		"!pkg/evaluation/Dockerfile.template", "!pkg/evaluation/Dockerfile.custom.template",
		"!pkg/modelsdev/snapshot.json", "!pkg/modelsdev/snapshot_date.txt",
		"!pkg/safety/safety_patterns.json", "!pkg/tools/builtin/mcpcatalog/servers.json",
		"!pkg/tui/styles/themes/*.yaml",
		"!kit/launch.sh", "!kit/hackerspace.yaml", "!kit/user-config.yaml",
		"**/*_test.go", "**/testdata/**", "**/fixtures/**",
	}, rules)
}

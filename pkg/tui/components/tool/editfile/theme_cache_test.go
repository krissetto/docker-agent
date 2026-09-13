package editfile

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tui/styles"
	"github.com/docker/docker-agent/pkg/tui/types"
)

func TestWarmEditFileCacheTracksThemeGeneration(t *testing.T) {
	original := styles.CurrentTheme()
	t.Cleanup(func() { styles.ApplyTheme(original); InvalidateCaches() })
	path := filepath.Join(t.TempDir(), "source.go")
	require.NoError(t, os.WriteFile(path, []byte("package main\nvar count = 1\n"), 0o600))
	args, err := json.Marshal(map[string]any{"path": path, "edits": []map[string]string{{"oldText": "count = 1", "newText": "count = 2"}}})
	require.NoError(t, err)
	call := tools.ToolCall{ID: t.Name(), Function: tools.FunctionCall{Name: "edit_file", Arguments: string(args)}}
	before := renderEditFile(call, 80, false, types.ToolStatusConfirmation)
	require.NotEmpty(t, before)
	require.Equal(t, before, renderEditFile(call, 80, false, types.ToolStatusConfirmation))
	generation := styles.ThemeGeneration()
	theme := *original
	theme.Colors.DiffAddBg, theme.Colors.DiffRemoveBg = "#123456", "#654321"
	styles.ApplyTheme(&theme)
	got := renderEditFile(call, 80, false, types.ToolStatusConfirmation)
	require.NotEqual(t, before, got)
	require.Equal(t, renderEditFileUncached(call, 80, false, types.ToolStatusConfirmation), got)
	c := getOrCreateCache(call.ID)
	cacheMu.Lock()
	defer cacheMu.Unlock()
	require.Greater(t, c.themeGeneration, generation)
	require.Equal(t, styles.ThemeGeneration(), c.themeGeneration)
}

func TestExplicitInvalidationRejectsInflightRenderPublication(t *testing.T) {
	call := tools.ToolCall{ID: t.Name()}
	generation := styles.ThemeGeneration()
	stale := renderEditFileCached(call, 80, false, types.ToolStatusCompleted,
		func(tools.ToolCall, int, bool, types.ToolStatus) string {
			InvalidateCaches()
			return "in-flight old output"
		})
	require.Equal(t, "in-flight old output", stale)
	require.Equal(t, generation, styles.ThemeGeneration(), "explicit invalidation need not change the theme")
	c := getOrCreateCache(call.ID)
	cacheMu.Lock()
	published := c.renderCached
	cacheMu.Unlock()
	require.False(t, published)
	calls := 0
	render := func(tools.ToolCall, int, bool, types.ToolStatus) string {
		calls++
		return "fresh output"
	}
	require.Equal(t, "fresh output", renderEditFileCached(call, 80, false, types.ToolStatusCompleted, render))
	require.Equal(t, "fresh output", renderEditFileCached(call, 80, false, types.ToolStatusCompleted, render))
	require.Equal(t, 1, calls, "the replacement render, not the invalidated one, becomes reusable")
}

package tui

import (
	"math"
	"os"
	"path/filepath"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/styles"
	"github.com/docker/docker-agent/pkg/userconfig"
)

func TestInactivePaneDimSettingsAppliesLiveAndPersists(t *testing.T) {
	setupSettingsConfigTest(t)
	root, _, _ := frozenClockRoot(t, 120, 50)
	require.InDelta(t, userconfig.DefaultInactivePaneDim, root.inactivePaneDim, 1e-12)

	d := settingsFromRoot(t, root)
	settingsClickRendered(t, d, "Inactive pane dim")
	_, cmd := d.Update(tea.KeyPressMsg{Code: tea.KeyRight})
	preview, ok := firstOfType[messages.PreviewSettingsMsg](collectMsgs(cmd))
	require.True(t, ok)
	root.handleSettingsPreview(preview)
	require.InDelta(t, 0.55, root.inactivePaneDim, 1e-12, "preview applies live")
	require.InDelta(t, 0.5, userconfig.Get().GetInactivePaneDim(), 1e-12, "preview must not persist")

	settingsSave(t, root, d)
	require.InDelta(t, 0.55, userconfig.Get().GetInactivePaneDim(), 1e-12)
	reopened, _, _ := frozenClockRoot(t, 120, 50)
	require.InDelta(t, 0.55, reopened.inactivePaneDim, 1e-12, "saved value is read on next launch")

	d = settingsFromRoot(t, reopened)
	settingsClickRendered(t, d, "Inactive pane dim")
	_, cmd = d.Update(tea.KeyPressMsg{Code: tea.KeyLeft})
	preview, ok = firstOfType[messages.PreviewSettingsMsg](collectMsgs(cmd))
	require.True(t, ok)
	reopened.handleSettingsPreview(preview)
	settingsSave(t, reopened, d)
	cfg := userconfig.Get()
	assert.Nil(t, cfg.InactivePaneDim, "the default is omitted from the saved config")
	assert.InDelta(t, 0.5, cfg.GetInactivePaneDim(), 1e-12)
}

func TestInactivePaneDimCancelRestoresRuntimeValue(t *testing.T) {
	setupSettingsConfigTest(t)
	root, _, _ := frozenClockRoot(t, 120, 50)
	d := settingsFromRoot(t, root)
	settingsClickRendered(t, d, "Inactive pane dim")
	_, cmd := d.Update(tea.KeyPressMsg{Code: tea.KeyRight})
	preview, ok := firstOfType[messages.PreviewSettingsMsg](collectMsgs(cmd))
	require.True(t, ok)
	root.handleSettingsPreview(preview)
	require.InDelta(t, 0.55, root.inactivePaneDim, 1e-12)
	settingsCancel(t, root, d)
	require.InDelta(t, 0.5, root.inactivePaneDim, 1e-12)
}

func TestInactivePaneDimControlsSplitPaneFadeStrength(t *testing.T) {
	root := splitTestRoot(t)
	root.splitPane("second", "profile", splitRight)
	root.dimInactivePanes = true
	input := "\x1b[38;2;221;212;201mtext\x1b[m"
	root.inactivePaneDim = 0.5
	atDefault := root.paneTranscript("profile", input)
	fade := styles.NewFadeContext()
	require.Equal(t, styles.FadeLineCtx(input, 0.5, &fade), atDefault)

	root.inactivePaneDim = 0.2
	stronger := root.paneTranscript("profile", input)
	require.NotEqual(t, atDefault, stronger, "a changed fade amount invalidates the retained transcript")
	require.Equal(t, styles.FadeLineCtx(input, 0.8, &fade), stronger)
	require.Equal(t, "text", ansi.Strip(stronger))

	root.inactivePaneDim = 0
	require.Equal(t, styles.FadeLineCtx(input, 1, &fade), root.paneTranscript("profile", input))
}

func TestInactivePaneDimClampsConfigAndRenderedEffect(t *testing.T) {
	setupSettingsConfigTest(t)
	for content, want := range map[string]float64{
		"settings:\n  inactive_pane_dim: 3\n":    1,
		"settings:\n  inactive_pane_dim: -2\n":   0,
		"settings:\n  inactive_pane_dim: .nan\n": userconfig.DefaultInactivePaneDim,
	} {
		require.NoError(t, os.MkdirAll(filepath.Dir(userconfig.Path()), 0o755))
		require.NoError(t, os.WriteFile(userconfig.Path(), []byte(content), 0o644))
		root, _, _ := frozenClockRoot(t, 120, 50)
		require.InDelta(t, want, root.inactivePaneDim, 1e-12, content)
	}

	root := splitTestRoot(t)
	root.splitPane("second", "profile", splitRight)
	root.dimInactivePanes = true
	input := "\x1b[38;2;221;212;201mtext\x1b[m"
	root.inactivePaneDim = 7
	fade := styles.NewFadeContext()
	require.Equal(t, styles.FadeLineCtx(input, 0, &fade), root.paneTranscript("profile", input), "out-of-range values render as the clamped bound")
}

func TestInactivePaneDimAbsentExplicitZeroAndClampedPersistence(t *testing.T) {
	setupSettingsConfigTest(t)
	require.NoError(t, os.MkdirAll(filepath.Dir(userconfig.Path()), 0o755))
	require.NoError(t, os.WriteFile(userconfig.Path(), []byte("settings:\n  theme: nord\n"), 0o644))

	// A pre-existing config without the key loads the default, not zero.
	root, _, _ := frozenClockRoot(t, 120, 50)
	require.InDelta(t, userconfig.DefaultInactivePaneDim, root.inactivePaneDim, 1e-12)
	require.Contains(t, ansi.Strip(settingsFromRoot(t, root).View()), "50%")

	// An explicit zero is a real value: it persists as 0 and reloads as 0.
	p := preferencesFromConfig(userconfig.Get())
	require.InDelta(t, userconfig.DefaultInactivePaneDim, p.InactivePaneDim, 1e-12)
	p.InactivePaneDim = 0
	require.NoError(t, savePreferences(p))
	raw, err := os.ReadFile(userconfig.Path())
	require.NoError(t, err)
	require.Contains(t, string(raw), "inactive_pane_dim: 0")
	cfg, err := userconfig.Load()
	require.NoError(t, err)
	require.NotNil(t, cfg.Settings.InactivePaneDim)
	require.InDelta(t, 0, cfg.GetSettings().GetInactivePaneDim(), 1e-12)
	reopened, _, _ := frozenClockRoot(t, 120, 50)
	require.InDelta(t, 0, reopened.inactivePaneDim, 1e-12)
	require.Contains(t, ansi.Strip(settingsFromRoot(t, reopened).View()), "0%")

	// Out-of-range values are clamped before they reach the file; non-finite values are omitted.
	p.InactivePaneDim = 3
	require.NoError(t, savePreferences(p))
	cfg, err = userconfig.Load()
	require.NoError(t, err)
	require.InDelta(t, 1, cfg.GetSettings().GetInactivePaneDim(), 1e-12)
	p.InactivePaneDim = math.NaN()
	require.NoError(t, savePreferences(p))
	raw, err = os.ReadFile(userconfig.Path())
	require.NoError(t, err)
	require.NotContains(t, string(raw), "inactive_pane_dim")
}

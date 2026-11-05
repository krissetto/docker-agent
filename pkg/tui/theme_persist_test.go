package tui

import (
	"testing"

	"charm.land/lipgloss/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/styles"
	"github.com/docker/docker-agent/pkg/userconfig"
)

// Choosing a Docker Next or black palette in the picker (double-click or Enter)
// reaches the root theme-change handler, which applies the palette and persists
// it. The round trip is checked against the saved config file in an isolated
// config directory; the default restore clears the saved setting as before, and
// the explicit Old School selection is saved under its own name.
func TestThemePersistsThroughRootThemeChange(t *testing.T) { //nolint:paralleltest // Mutates package-wide theme state and the config directory.
	setupAutoThemeTest(t)
	m, _ := newTestModel(t)

	for _, tc := range []struct {
		ref       string
		wantSaved string
		wantMuted string
	}{
		{"docker-next-light", "docker-next-light", "#64605C"},
		{"black-amber", "black-amber", "#A4B2C2"},
		{styles.LegacyDefaultThemeRef, styles.LegacyDefaultThemeRef, "#8f8f8f"},
		{styles.DefaultThemeRef, "", "#97918A"},
	} {
		_, _ = m.handleChangeTheme(tc.ref)

		cfg, err := userconfig.Load()
		require.NoError(t, err, tc.ref)
		require.NotNil(t, cfg.Settings, tc.ref)
		assert.Equal(t, tc.wantSaved, cfg.Settings.Theme, "saved theme for %s", tc.ref)
		assert.Equal(t, tc.ref, styles.GetPersistedThemeRef(), "persisted ref for %s", tc.ref)
		assert.Equal(t, lipgloss.Color(tc.wantMuted), styles.TextMuted, "applied palette for %s", tc.ref)
	}
}

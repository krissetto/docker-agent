package styles_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/styles"
)

func TestApplyThemeRef(t *testing.T) {
	original := styles.CurrentTheme()
	t.Cleanup(func() { styles.ApplyTheme(original) })

	theme := styles.ApplyThemeRef(styles.DefaultThemeRef)
	require.NotNil(t, theme)
	assert.Equal(t, styles.DefaultThemeRef, theme.Ref)
	assert.Equal(t, theme.Ref, styles.CurrentTheme().Ref)

	// Unknown refs fall back to the default theme instead of failing.
	fallback := styles.ApplyThemeRef("no-such-theme")
	require.NotNil(t, fallback)
	assert.Equal(t, styles.DefaultTheme().Name, fallback.Name)
}

func TestOnThemeChange(t *testing.T) {
	original := styles.CurrentTheme()
	t.Cleanup(func() { styles.ApplyTheme(original) })

	calls := 0
	styles.OnThemeChange(func() { calls++ })

	styles.ApplyTheme(styles.DefaultTheme())
	assert.Equal(t, 1, calls)

	styles.ApplyThemeRef(styles.DefaultThemeRef)
	assert.Equal(t, 2, calls)
}

func TestThemeGenerationAdvancesOnEveryApplication(t *testing.T) { //nolint:paralleltest // theme globals
	original := styles.CurrentTheme()
	t.Cleanup(func() { styles.ApplyTheme(original) })
	before := styles.ThemeGeneration()
	styles.ApplyTheme(styles.DefaultTheme())
	assert.Greater(t, styles.ThemeGeneration(), before)
	before = styles.ThemeGeneration()
	styles.ApplyTheme(styles.DefaultTheme())
	assert.Greater(t, styles.ThemeGeneration(), before, "reapplying a changed in-memory palette also invalidates caches")
}

package spinner

import (
	"testing"

	"charm.land/lipgloss/v2"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

func TestWarmSpinnerThemeRefreshPreservesSharedClock(t *testing.T) {
	original := styles.CurrentTheme()
	t.Cleanup(func() { styles.ApplyTheme(original) })
	ar := animation.NewRuntime()
	s := New(ar, ModeBoth, styles.SpinnerDotsAccentStyle).(*spinner)
	s.SetMessage("Working")
	require.NotNil(t, s.Init())
	t.Cleanup(s.Stop)
	before, raw, pos := s.View(), s.RawFrame(), s.lightPosition
	custom := New(ar, ModeSpinnerOnly, lipgloss.NewStyle().Foreground(lipgloss.Color("#123456")))
	customBefore := custom.View()
	theme := *original
	theme.Colors.Accent = "#112233"
	theme.Colors.SpinnerBrightest, theme.Colors.SpinnerBright, theme.Colors.SpinnerDim = "#223344", "#334455", "#445566"
	theme.Colors.TextMuted = "#556677"
	styles.ApplyTheme(&theme)
	got := s.View()
	require.NotEqual(t, before, got)
	require.Equal(t, raw, s.RawFrame())
	require.Equal(t, pos, s.lightPosition)
	require.True(t, ar.HasActive())
	fresh := New(ar, ModeBoth, styles.SpinnerDotsAccentStyle).(*spinner)
	fresh.SetMessage("Working")
	require.Equal(t, fresh.View(), got)
	require.Equal(t, customBefore, custom.View(), "explicit caller styles remain explicit")
	s.Stop()
	require.False(t, ar.HasActive())
}

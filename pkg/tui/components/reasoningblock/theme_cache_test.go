package reasoningblock

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

func TestWarmReasoningAndFadeCachesFollowThemeGeneration(t *testing.T) {
	original := styles.CurrentTheme()
	t.Cleanup(func() { styles.ApplyTheme(original) })
	m := New(animation.NewRuntime(), "theme", "root", &service.SessionState{})
	m.SetReasoning("stable **reasoning**")
	oldCache := m.ensureCache()
	before := m.fadeStyleForProgress(0.5).Render("fading tool")
	theme := *original
	theme.Colors.Background, theme.Colors.TextMuted = "#ffffff", "#112233"
	styles.ApplyTheme(&theme)
	require.NotSame(t, oldCache, m.ensureCache())
	require.Equal(t, oldCache.lines, m.ensureCache().lines, "reasoning cache contains plain text")
	require.NotEqual(t, before, m.fadeStyleForProgress(0.5).Render("fading tool"))
	fresh := New(animation.NewRuntime(), "fresh", "root", &service.SessionState{})
	require.Equal(t, fresh.fadeStyleForProgress(0.5), m.fadeStyleForProgress(0.5))
}

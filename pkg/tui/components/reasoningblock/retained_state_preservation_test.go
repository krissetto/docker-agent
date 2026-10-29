package reasoningblock

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/service"
)

func TestFadePaletteIsSharedAcrossBlocks(t *testing.T) {
	a := New(animation.NewRuntime(), "palette-a", "root", service.StaticSessionState{})
	b := New(animation.NewRuntime(), "palette-b", "root", service.StaticSessionState{})
	require.Equal(t, a.fadeStyleForProgress(0.5).Render("fading"), b.fadeStyleForProgress(0.5).Render("fading"))
	require.Same(t, currentFadePalette(), fadePaletteCache.Load())
}

func TestCollapsedPreparedReasoningKeepsNoChunksAndMatchesView(t *testing.T) {
	content := strings.Repeat("reasoning paragraph **bold** text\n\n", 600)
	prepared := New(animation.NewRuntime(), "prepared", "root", service.StaticSessionState{})
	prepared.SetReasoning(content)
	prepared.SetSize(80, 30)
	prepare := prepared.PrepareReasoning()
	require.NotNil(t, prepare)
	result := prepare()
	require.Nil(t, result.chunks)
	require.True(t, prepared.ApplyPreparedReasoning(result))

	fresh := New(animation.NewRuntime(), "fresh", "root", service.StaticSessionState{})
	fresh.SetReasoning(content)
	fresh.SetSize(80, 30)
	require.Equal(t, fresh.View(), prepared.View())
}

// Expansion is block state; resizing with a prepared expanded snapshot must fall back to a fresh render.
func TestReasoningExpansionSurvivesResizeAndPreparedSnapshot(t *testing.T) {
	content := strings.Repeat("expanded reasoning paragraph **bold** text\n\n", 600)
	live := New(animation.NewRuntime(), "expand-live", "root", service.StaticSessionState{})
	live.SetReasoning(content)
	live.SetSize(80, 30)
	live.SetExpanded(true)
	prepare := live.PrepareReasoning()
	require.NotNil(t, prepare)
	require.True(t, live.ApplyPreparedReasoning(prepare()))

	live.SetSize(60, 30)
	oracle := New(animation.NewRuntime(), "expand-oracle", "root", service.StaticSessionState{})
	oracle.SetReasoning(content)
	oracle.SetSize(60, 30)
	oracle.SetExpanded(true)
	require.True(t, live.IsExpanded())
	require.Equal(t, oracle.View(), live.View(), "expanded view after resize")

	live.SetExpanded(false)
	oracle.SetExpanded(false)
	require.Equal(t, oracle.View(), live.View(), "collapsed view after expansion round trip")
}

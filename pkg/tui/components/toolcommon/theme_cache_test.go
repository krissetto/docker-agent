package toolcommon

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/components/spinner"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/styles"
	"github.com/docker/docker-agent/pkg/tui/types"
)

func TestPendingToolWarmFallbackDoesNotRetainOldTheme(t *testing.T) {
	original := styles.CurrentTheme()
	t.Cleanup(func() { styles.ApplyTheme(original) })
	content := "full argument body\nsecond line"
	render := func(*types.Message, spinner.Spinner, service.SessionStateReader, int, int) string {
		return styles.MutedStyle.Render(content)
	}
	b := NewBaseWithCollapsed(animation.NewRuntime(), &types.Message{ToolStatus: types.ToolStatusPending}, &service.SessionState{}, render, CollapsedRenderer(render))
	full, collapsed := b.View(), b.CollapsedView()
	content = "partial"
	require.Equal(t, full, b.View())
	require.Equal(t, collapsed, b.CollapsedView())
	theme := *original
	theme.Colors.TextMuted = "#123456"
	styles.ApplyTheme(&theme)
	want := styles.MutedStyle.Render(content)
	require.Equal(t, want, b.View())
	require.Equal(t, want, b.CollapsedView())
	require.NotEqual(t, want, full)
}

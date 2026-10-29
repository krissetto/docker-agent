package chat

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/runtime"
	msgtypes "github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

// TestHandleMouseClick_SidebarUsageContext_OpensContextDialog verifies that a
// click on the token/context part of the sidebar's Token Usage reading opens
// the context dialog rather than the cost dialog.
func TestHandleMouseClick_SidebarUsageContext_OpensContextDialog(t *testing.T) {
	t.Parallel()

	p := newLayoutTestPage(t, msgtypes.SidebarRight)
	setSidebarUsageFixture(p)
	sl := p.computeSidebarLayout()
	require.Equal(t, sidebarVertical, sl.mode)

	// Force a render so the sidebar records the usage click zone.
	view := ansi.Strip(p.sidebar.View())
	line, _ := findUsageReadingLine(t, view)

	hit := NewHitTest(p)
	// Offset 1 sits on the token glyph itself, safely within the context
	// segment (glyph, token count, and any context %/compacting marker).
	target := hit.At(styles.AppPadding+sl.sidebarStartX+1, line)
	assert.Equal(t, TargetSidebarUsageContext, target, "a click on the context segment should target the context dialog")
}

// TestHandleMouseClick_SidebarUsage_OpensCostDialog mirrors the context-dialog
// test for the cost segment ($ and onward), which keeps opening /cost.
func TestHandleMouseClick_SidebarUsage_OpensCostDialog(t *testing.T) {
	t.Parallel()

	p := newLayoutTestPage(t, msgtypes.SidebarRight)
	setSidebarUsageFixture(p)
	sl := p.computeSidebarLayout()
	require.Equal(t, sidebarVertical, sl.mode)

	view := ansi.Strip(p.sidebar.View())
	line, dollarOffset := findUsageReadingLine(t, view)

	hit := NewHitTest(p)
	target := hit.At(styles.AppPadding+sl.sidebarStartX+dollarOffset, line)
	assert.Equal(t, TargetSidebarUsage, target, "a click on the cost segment should keep targeting the cost dialog")
}

// findUsageReadingLine locates the Token Usage reading line ("◉ ... $...")
// in the stripped sidebar view and returns its content-line index and the
// offset of the "$" within it.
func findUsageReadingLine(t *testing.T, strippedView string) (line, dollarOffset int) {
	t.Helper()
	for i, l := range strings.Split(strippedView, "\n") {
		if idx := strings.Index(l, "$"); idx >= 0 && strings.Contains(l, "◉") {
			return i, idx
		}
	}
	t.Fatal("Token Usage reading line not found in sidebar view")
	return 0, 0
}

func setSidebarUsageFixture(p *chatPage) {
	p.sidebar.SetTokenUsage(&runtime.TokenUsageEvent{
		SessionID:    "fixture-session",
		AgentContext: runtime.AgentContext{AgentName: "root"},
		Usage:        &runtime.Usage{},
	})
}

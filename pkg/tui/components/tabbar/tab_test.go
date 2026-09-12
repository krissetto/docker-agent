package tabbar

import (
	"strings"
	"testing"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"

	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/messages"
)

func TestRenderTabAttachedIdentityAndRootUnchanged(t *testing.T) {
	t.Parallel()
	root := renderTab(messages.TabInfo{Title: "worker"}, 20, dragRoleNone, 0)
	attached := renderTab(messages.TabInfo{Title: "worker", IsAttached: true}, 20, dragRoleNone, 0)

	assert.Equal(t, "▎ worker × ", ansi.Strip(root.View()), "ordinary root rendering stays visually stable")
	assert.NotContains(t, ansi.Strip(root.View()), attachedIndicator)
	assert.Contains(t, ansi.Strip(attached.View()), attachedIndicator)
	assert.Equal(t, root.Width()+lipgloss.Width(attachedIndicator), attached.Width())
	assert.Equal(t, root.MainZoneEnd()+lipgloss.Width(attachedIndicator), attached.MainZoneEnd(),
		"identity marker expands only the main click zone")
}

func TestRenderTabActivityVisibleWhenActiveAndAttentionWins(t *testing.T) {
	t.Parallel()
	active := renderTab(messages.TabInfo{Title: "active", IsActive: true, Activity: messages.TabActivityRunning}, 20, dragRoleNone, 3*animation.Card.DefaultFrameDuration())
	assert.Contains(t, ansi.Strip(active.View()), animation.Card.FrameAt(3*animation.Card.DefaultFrameDuration())+" ", "active tabs still expose work")

	descendant := renderTab(messages.TabInfo{Title: "parent", Activity: messages.TabActivityDescendantRunning}, 20, dragRoleNone, 0)
	assert.Contains(t, ansi.Strip(descendant.View()), descendantIndicator)
	assert.NotContains(t, ansi.Strip(descendant.View()), animation.Card.FrameAt(0*time.Second))

	pending := renderTab(messages.TabInfo{Title: "pending", Activity: messages.TabActivityPending}, 20, dragRoleNone, 0)
	assert.Contains(t, ansi.Strip(pending.View()), pendingIndicator)
	assert.NotContains(t, ansi.Strip(pending.View()), animation.Card.FrameAt(0*time.Second))

	attention := renderTab(messages.TabInfo{Title: "alert", IsAttached: true, Activity: messages.TabActivityRunning, NeedsAttention: true}, 20, dragRoleNone, 0)
	attentionPlain := ansi.Strip(attention.View())
	assert.Contains(t, attentionPlain, attentionIndicator)
	assert.NotContains(t, attentionPlain, animation.Card.FrameAt(0*time.Second))
	assert.Equal(t, 1, strings.Count(attentionPlain, attentionIndicator))

	descendantAttention := renderTab(messages.TabInfo{Title: "parent", Activity: messages.TabActivityDescendantRunning, NeedsAttention: true}, 20, dragRoleNone, 0)
	plain := ansi.Strip(descendantAttention.View())
	assert.Contains(t, plain, descendantAttentionIndicator, "combined glyph keeps descendant work apparent under attention")
	assert.NotContains(t, plain, attentionIndicator)

	for _, glyph := range []string{strings.TrimSpace(attachedIndicator), strings.TrimSpace(pendingIndicator), strings.TrimSpace(descendantIndicator), strings.TrimSpace(descendantAttentionIndicator), strings.TrimSpace(attentionIndicator)} {
		assert.Equal(t, 1, lipgloss.Width(glyph), "%q must occupy one cell", glyph)
	}
}

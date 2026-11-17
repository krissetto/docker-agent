package messages

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/types"
)

func TestClassifiedGenericRuntimeNoticeSurvivesReload(t *testing.T) {
	input := session.UserMessage("\u23f0 Scheduled task \"nightly\" (s1) is due:\nrun the audit")
	input.InputOrigin, input.InputMode = session.InputOriginRuntime, "steer"
	sess := session.New()
	sess.AddMessage(input)

	live := NewScrollableView(animation.NewRuntime(), 80, 40, &service.SessionState{}).(*model)
	live.AddInputMessage(input, 0)
	restored := NewScrollableView(animation.NewRuntime(), 80, 40, &service.SessionState{}).(*model)
	restored.LoadFromSession(sess, nil)

	for _, m := range []*model{live, restored} {
		require.Len(t, m.messages, 1)
		assert.Equal(t, types.MessageTypeRuntimeNotice, m.messages[0].Type)
		m.ensureAllItemsRendered()
		out := ansi.Strip(strings.Join(m.renderedLines, "\n"))
		assert.Contains(t, out, "Scheduled task due >")
		assert.NotContains(t, out, "run the audit")
		view := m.views[0].(interface{ SetExpanded(bool) })
		view.SetExpanded(true)
		m.invalidateItem(0)
		m.ensureAllItemsRendered()
		assert.Contains(t, ansi.Strip(strings.Join(m.renderedLines, "\n")), "run the audit")
	}
}

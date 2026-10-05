package subagenttool

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/subagent"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/styles"
	"github.com/docker/docker-agent/pkg/tui/types"
)

func TestFailedSendHeaderKeepsStatusAndErrorWhenExpanded(t *testing.T) {
	const detail = "Cannot send message: subagent engineer (43140) has stopped."
	msg := testMessage("send_message", `{"to":"43140-child","message":"Please continue."}`, detail, types.ToolStatusError)
	view := NewSend(animation.NewRuntime(), msg, nil, func(subagent.NodeID) (string, bool) { return "engineer", true }).(*sendModel)
	defer view.StopAnimation()
	for _, expanded := range []bool{false, true} {
		view.SetExpanded(expanded)
		out := view.View()
		chevron := ">"
		if expanded {
			chevron = "v"
		}
		require.Contains(t, ansi.Strip(out), "✗ Messaging engineer (43140) "+chevron)
		iconStyle, _, _ := strings.Cut(styles.ToolErrorIcon.Render("✗"), "✗")
		verbStyle, _, _ := strings.Cut(styles.MutedStyle.Render("Messaging"), "Messaging")
		identityStyle, _, _ := strings.Cut(styles.AgentIdentityStyle("engineer", false).Render("engineer"), "engineer")
		require.Contains(t, out, strings.TrimLeft(iconStyle, " ")+"✗")
		require.Contains(t, out, verbStyle+"Messaging")
		require.Contains(t, out, identityStyle+"engineer")
		require.Contains(t, ansi.Strip(out), detail)
		require.Equal(t, 1, strings.Count(ansi.Strip(out), "Messaging"))
		hits := 0
		for y, line := range strings.Split(out, "\n") {
			for x := range ansi.StringWidth(line) {
				if view.IsToggleAt(y, x) {
					hits++
					require.Equal(t, chevron, ansi.Strip(ansi.Cut(line, x, x+1)))
				}
			}
		}
		require.Equal(t, 1, hits, "only the header chevron toggles; the error remains text")
	}
}

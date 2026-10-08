package subagenttool

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/app/lifecycle"
	"github.com/docker/docker-agent/pkg/subagent"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/components/agentidentity"
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
		for y := range strings.Split(out, "\n") {
			for x := range 80 {
				if view.IsToggleAt(y, x) {
					hits++
					require.True(t, view.InputReferenceOnLine(y))
				}
			}
		}
		require.Equal(t, 80, hits, "the whole header toggles; the error remains text")
	}
}

func TestSendParentHeaderUsesSharedIdentityInBothDisclosureStates(t *testing.T) {
	ref := lifecycle.InputReference{Kind: lifecycle.InputReferenceParent, ID: "parent-session-full", Name: "root", Agent: "root", DisplayID: "abcde"}
	msg := testMessage("send_message", `{"to":"parent","message":"payload"}`, "Message delivered to parent.", types.ToolStatusCompleted)
	view := NewSend(animation.NewRuntime(), msg, nil, nil, func(id subagent.NodeID) lifecycle.InputReference {
		require.Equal(t, subagent.NodeID(subagent.ParentAlias), id)
		return ref
	}).(*sendModel)
	defer view.StopAnimation()
	for _, expanded := range []bool{false, true} {
		view.SetExpanded(expanded)
		header := strings.Split(view.View(), "\n")[0]
		require.Contains(t, header, styles.AgentIdentityStyle("root", false).Render("root"))
		require.Contains(t, header, ansi.SetHyperlink(agentidentity.Link, "id=docker-agent-identity-name"))
		require.NotContains(t, ansi.Strip(header), "Messaged parent")
		resolved, ok := view.InputReferenceForLine(0)
		require.True(t, ok)
		require.Equal(t, ref, resolved)
		require.Contains(t, ansi.Strip(view.CollapsedView()), "Messaged root (abcde)", "outer reasoning summary keeps the real parent identity too")
	}
}

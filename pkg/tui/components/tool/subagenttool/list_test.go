package subagenttool

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/app/lifecycle"
	"github.com/docker/docker-agent/pkg/subagent"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/types"
)

func listResult(children []listedAgentRef, next string) string {
	result, err := json.Marshal(listedAgentsResult{CurrentAgent: "root", Children: children, NextCursor: next})
	if err != nil {
		panic(err)
	}
	return string(result)
}

func listMessage(result string, status types.ToolStatus) *types.Message {
	msg := testMessage(subagent.ToolListAgents, `{"limit":10}`, result, status)
	msg.Content = result
	return msg
}

func TestListAgentsCompactAndExpandedRoster(t *testing.T) {
	msg := listMessage(listResult([]listedAgentRef{{ID: "abcde", Agent: "coder", Name: "implementation", SessionTitle: "Implement project entry point", State: string(subagent.NodeRunning)}}, "abcde"), types.ToolStatusCompleted)
	view := NewList(animation.NewRuntime(), msg, nil, func(id subagent.NodeID) lifecycle.InputReference {
		return lifecycle.InputReference{Kind: lifecycle.InputReferenceNode, ID: string(id), Name: "implementation", Agent: "coder", DisplayID: "abcde"}
	}).(*listAgentsModel)
	defer view.StopAnimation()
	view.SetSize(100, 0)

	collapsed := ansi.Strip(view.View())
	require.Contains(t, collapsed, "Listed 1 agent · 1 running")
	require.NotContains(t, collapsed, "implementation")
	require.NotContains(t, collapsed, "{\"current_agent\"")

	view.SetExpanded(true)
	view.SetSize(40, 0)
	expanded := ansi.Strip(view.View())
	require.Contains(t, expanded, "root")
	require.Contains(t, expanded, "implementation (abcde)")
	require.Contains(t, expanded, "Implement\nproject entry point")
	require.NotContains(t, expanded, "running ·")
	require.NotContains(t, expanded, "Own the implementation")
	require.NotContains(t, expanded, "raw JSON")
	require.NotContains(t, expanded, "next_cursor")
	require.NotContains(t, expanded, "\"children\"")
	ref, ok := view.InputReferenceForLine(2) // line 1 is the current-agent context row; line 2 is the child row
	require.True(t, ok)
	require.Equal(t, "abcde", ref.ID)

	for _, width := range []int{24, 12, 5, 100} {
		view.SetSize(width, 0)
		for line := range strings.SplitSeq(view.View(), "\n") {
			require.LessOrEqual(t, ansi.StringWidth(line), width, "width %d: %q", width, ansi.Strip(line))
		}
	}

	// Pagination is indicated in plain text and never becomes a link.
	for line, rendered := range strings.Split(view.View(), "\n") {
		if strings.Contains(ansi.Strip(rendered), "Next page") {
			_, ok := view.InputReferenceForLine(line)
			require.False(t, ok)
		}
	}
}

func TestListAgentsTreePrefersAgentPageAndShowsReadOnly(t *testing.T) {
	msg := listMessage(`{"scope":"tree","current_agent":"root","children":[],"agents":[{"id":"abcde","agent":"worker","name":"research","state":"idle","messageable":false,"parent_id":"f0000"}],"next_cursor":"abcde"}`, types.ToolStatusCompleted)
	view := NewList(animation.NewRuntime(), msg, nil).(*listAgentsModel)
	defer view.StopAnimation()
	view.SetExpanded(true)
	plain := ansi.Strip(view.View())
	require.Contains(t, plain, "Listed 1 agent in tree")
	require.Contains(t, plain, "research (abcde)")
	require.Contains(t, plain, "read-only")
	require.NotContains(t, plain, "parent f0000")
	require.NotContains(t, plain, "idle")
	// Without an index, an otherwise valid ID still cannot navigate.
	_, ok := view.InputReferenceForLine(2)
	require.False(t, ok)
}

func TestListAgentsDefensivelyBoundsOversizedPages(t *testing.T) {
	children := make([]listedAgentRef, 75)
	for i := range children {
		children[i] = listedAgentRef{ID: fmt.Sprintf("%05x", i), Agent: "worker", Name: fmt.Sprintf("worker-%d", i), State: string(subagent.NodeIdle)}
	}
	msg := listMessage(listResult(children, "00049"), types.ToolStatusCompleted)
	view := NewList(animation.NewRuntime(), msg, nil).(*listAgentsModel)
	defer view.StopAnimation()
	view.SetExpanded(true)
	plain := ansi.Strip(view.View())
	require.Contains(t, plain, "worker-49")
	require.NotContains(t, plain, "worker-50")
	require.LessOrEqual(t, len(strings.Split(plain, "\n")), maxListAgentsRows+4)
}

func TestListAgentsNavigationRequiresCanonicalChildAndSettledDisclosure(t *testing.T) {
	msg := listMessage(listResult([]listedAgentRef{{ID: "abcde", Agent: "worker", Name: "worker", State: string(subagent.NodeIdle)}}, ""), types.ToolStatusCompleted)
	view := NewList(animation.NewRuntime(), msg, nil, func(id subagent.NodeID) lifecycle.InputReference {
		return lifecycle.InputReference{Kind: lifecycle.InputReferenceNode, ID: string(id), Name: "worker", Agent: "worker"}
	}).(*listAgentsModel)
	defer view.StopAnimation()
	view.SetExpanded(true)
	view.View()
	_, ok := view.InputReferenceForLine(2)
	require.True(t, ok)
	view.Toggle()
	_, ok = view.InputReferenceForLine(2)
	require.False(t, ok, "navigation is disabled during the disclosure animation")
	view.StopAnimation()
	view.SetExpanded(true)
	view.parsed.Children[0].ID = "not-a-node"
	view.View()
	_, ok = view.InputReferenceForLine(2)
	require.False(t, ok, "noncanonical IDs never become navigation targets")
}

func TestListAgentsMalformedAndErrorResultsNeverExposeJSON(t *testing.T) {
	for _, test := range []struct {
		name   string
		status types.ToolStatus
		output string
		want   string
	}{
		{name: "malformed", status: types.ToolStatusCompleted, output: `{"opaque":"private raw payload"}`, want: "Agent list unavailable"},
		{name: "error", status: types.ToolStatusError, output: `{"error":"private detail"}`, want: "Agent listing failed"},
		{name: "empty", status: types.ToolStatusCompleted, output: "", want: "Agent list unavailable"},
	} {
		t.Run(test.name, func(t *testing.T) {
			msg := listMessage(test.output, test.status)
			view := NewList(animation.NewRuntime(), msg, nil).(*listAgentsModel)
			defer view.StopAnimation()
			view.SetExpanded(true)
			plain := ansi.Strip(view.View())
			require.Contains(t, plain, test.want)
			require.NotContains(t, plain, "raw JSON")
			require.NotContains(t, plain, "private")
			require.NotContains(t, plain, `"opaque"`)
			require.NotContains(t, plain, `"error"`)
		})
	}
}

func TestListAgentsMalformedAndUnsupportedShapesStayHumanReadable(t *testing.T) {
	for _, input := range []string{"not json", `{}`, `{"current_agent":"root"}`, `{"agents":[]}`} {
		t.Run(input, func(t *testing.T) {
			msg := listMessage(input, types.ToolStatusCompleted)
			view := NewList(animation.NewRuntime(), msg, nil).(*listAgentsModel)
			defer view.StopAnimation()
			view.SetExpanded(true)
			plain := ansi.Strip(view.View())
			require.Contains(t, plain, "Agent list unavailable")
			require.NotContains(t, plain, input)
			require.NotContains(t, plain, "{")
			require.NotContains(t, plain, "raw JSON")
		})
	}
}

func TestListAgentsPluralAndNarrowRoster(t *testing.T) {
	msg := listMessage(listResult([]listedAgentRef{
		{ID: "abcde", Agent: "one", State: string(subagent.NodeRunning)},
		{ID: "f0000", Agent: "two", State: string(subagent.NodeRunning)},
	}, ""), types.ToolStatusCompleted)
	view := NewList(animation.NewRuntime(), msg, nil).(*listAgentsModel)
	defer view.StopAnimation()
	require.Contains(t, ansi.Strip(view.View()), "Listed 2 agents · 2 running")
	view.SetExpanded(true)
	for _, width := range []int{40, 8, 3, 1} {
		view.SetSize(width, 0)
		for line := range strings.SplitSeq(view.View(), "\n") {
			require.LessOrEqual(t, ansi.StringWidth(line), width, "width %d: %q", width, ansi.Strip(line))
		}
	}
}

func TestListAgentsSessionTitlesSuppressDescriptionsAndCatalog(t *testing.T) {
	msg := listMessage(`{"current_agent":"root","scope":"children","self":{"id":"root:session-123","agent":"root-role","name":"Coordinator","session_title":" Coordinate safe rollout "},"children":[{"id":"abcde","agent":"worker-role","name":"Patch review","session_title":"Review auth flow","description":"SECRET description","state":"running"}],"catalog":[{"name":"configured-only","description":"SECRET catalog"}]}`, types.ToolStatusCompleted)
	view := NewList(animation.NewRuntime(), msg, nil).(*listAgentsModel)
	defer view.StopAnimation()
	view.SetExpanded(true)
	plain := ansi.Strip(view.View())
	require.Contains(t, plain, "Coordinator")
	require.Contains(t, plain, "Coordinate safe rollout")
	require.Contains(t, plain, "Patch review (abcde)")
	require.Contains(t, plain, "Review auth flow")
	require.NotContains(t, plain, "SECRET")
	require.NotContains(t, plain, "worker-role")
	require.NotContains(t, plain, "configured-only")
	require.NotContains(t, plain, "catalog")
	require.NotContains(t, plain, "running ·")
}

func TestListAgentsSelfParentTitlesAndSyntheticRootID(t *testing.T) {
	require.Equal(t, "root", displayAgentID("root:"))
	msg := listMessage(`{"scope":"tree","self":{"id":"root:session-1234567890","agent":"root-role","name":"Coordinator","session_title":"Root session title"},"parent":{"id":"abcde","agent":"parent-role","name":"Parent worker","session_title":"Parent session title"},"agents":[{"id":"root:session-1234567890","agent":"root-role","name":"Coordinator","session_title":"Root session title","state":"idle"},{"id":"abcde","agent":"parent-role","name":"Parent worker","session_title":"Parent session title","state":"running"},{"id":"f0000","agent":"child-role","name":"Child worker","session_title":"Child session title","state":"running"}]}`, types.ToolStatusCompleted)
	view := NewList(animation.NewRuntime(), msg, nil).(*listAgentsModel)
	defer view.StopAnimation()
	view.SetExpanded(true)
	plain := ansi.Strip(view.View())
	require.Contains(t, plain, "Coordinator (root:34567890)")
	require.Contains(t, plain, "Root session title")
	require.Contains(t, plain, "Parent worker (abcde)")
	require.Contains(t, plain, "Parent session title")
	require.Contains(t, plain, "Child worker (f0000)")
	require.Equal(t, 1, strings.Count(plain, "Root session title"))
	require.Equal(t, 1, strings.Count(plain, "Parent session title"))
	require.NotContains(t, plain, "(root:)")

	// Synthetic root identities are displayed usefully but are never node links.
	_, ok := view.InputReferenceForLine(1)
	require.False(t, ok)
}

func TestListAgentsReadOnlyMarkerFitsNarrowWidths(t *testing.T) {
	msg := listMessage(`{"current_agent":"root","children":[{"id":"abcde","agent":"worker","name":"worker","session_title":"A useful title","state":"running","messageable":false}]}`, types.ToolStatusCompleted)
	view := NewList(animation.NewRuntime(), msg, nil).(*listAgentsModel)
	defer view.StopAnimation()
	view.SetExpanded(true)
	for _, tc := range []struct {
		width  int
		marker string
	}{{1, "R"}, {2, "🔒"}, {3, "🔒"}, {12, "read-only"}} {
		view.SetSize(tc.width, 0)
		output := view.View()
		plain := ansi.Strip(output)
		flat := strings.ReplaceAll(plain, "\n", "")
		require.Contains(t, flat, tc.marker, "width %d", tc.width)
		if tc.width >= 12 {
			require.Contains(t, flat, "Auseful title", "width %d", tc.width)
		}
		for line := range strings.SplitSeq(output, "\n") {
			require.LessOrEqual(t, ansi.StringWidth(line), tc.width, "width %d: %q", tc.width, ansi.Strip(line))
		}
	}
}

func TestListAgentsLiveUpdateMatchesReplayAndOldPayload(t *testing.T) {
	first := `{"current_agent":"root","children":[{"id":"abcde","agent":"worker","name":"worker","state":"idle","description":"legacy secret"}]}`
	second := `{"current_agent":"root","children":[{"id":"abcde","agent":"worker","name":"worker","session_title":"Updated title","state":"failed","description":"legacy secret"}],"next_cursor":"abcde"}`

	live := NewList(animation.NewRuntime(), listMessage(first, types.ToolStatusCompleted), nil).(*listAgentsModel)
	defer live.StopAnimation()
	live.SetExpanded(true)
	oldOutput := ansi.Strip(live.View())
	require.Contains(t, oldOutput, "worker (abcde)")
	require.NotContains(t, oldOutput, "legacy secret")
	require.NotContains(t, oldOutput, "Updated title")

	live.msg.Content = second
	updated := ansi.Strip(live.View())
	require.Contains(t, updated, "Updated title")
	require.Contains(t, updated, "Next page available")
	require.NotContains(t, updated, "Next page · cursor")
	require.NotContains(t, updated, "legacy secret")

	replay := NewList(animation.NewRuntime(), listMessage(second, types.ToolStatusCompleted), nil).(*listAgentsModel)
	defer replay.StopAnimation()
	replay.SetExpanded(true)
	require.Equal(t, updated, ansi.Strip(replay.View()))
}

func TestListAgentsSanitizesUntrustedTextAndStaysNarrow(t *testing.T) {
	msg := listMessage(`{"current_agent":"root\u001b[2J","children":[{"id":"abcde","agent":"worker","name":"bad\u001b[31mname\u001b[0m\u202e","session_title":"title\nwith\ttabs\u2028and bidi","state":"running","description":"hidden"}]}`, types.ToolStatusCompleted)
	view := NewList(animation.NewRuntime(), msg, nil).(*listAgentsModel)
	defer view.StopAnimation()
	view.SetExpanded(true)
	for _, width := range []int{40, 12, 5, 1} {
		view.SetSize(width, 0)
		output := view.View()
		plain := ansi.Strip(output)
		require.NotContains(t, plain, "\x1b[2J")
		require.NotContains(t, plain, "\u202e")
		require.NotContains(t, plain, "\u2028")
		for line := range strings.SplitSeq(output, "\n") {
			require.LessOrEqual(t, ansi.StringWidth(line), width, "width %d: %q", width, ansi.Strip(line))
		}
	}
}

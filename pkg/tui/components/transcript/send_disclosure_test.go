package transcript

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/types"
)

func TestSendDisclosureSurvivesToolStatusReplacement(t *testing.T) {
	tr := newTestTranscript()
	defer tr.StopAnimations()
	call := tools.ToolCall{ID: "send", Function: tools.FunctionCall{Name: "send_message", Arguments: `{"to":"parent","message":"complete request body"}`}}
	tr.AddOrUpdateToolCall(testAgent, call, tools.Tool{}, types.ToolStatusRunning)
	view := tr.views[0].(interface{ SetExpanded(expanded bool) })
	view.SetExpanded(true)
	require.Contains(t, tr.Render(80), "complete request body")
	_, found := tr.SetToolStatus("send", types.ToolStatusCompleted)
	require.True(t, found)
	require.Contains(t, tr.Render(80), "complete request body")
	require.Len(t, tr.msgs, 1)
	require.Zero(t, tr.ar.ActiveCount(), "terminal replacement releases old spinner and transition")
	require.Equal(t, call.ID, tr.msgs[0].ToolCall.ID)
	require.Contains(t, tr.Render(28), "complete request")
	animation.StopView(tr.views[0])
}

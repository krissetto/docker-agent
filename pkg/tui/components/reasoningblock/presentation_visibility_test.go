package reasoningblock

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/types"
)

func TestResumeAnimationPreservesFiniteFadeDeadline(t *testing.T) {
	ar := animation.NewRuntime()
	t.Cleanup(ar.Stop)
	block := New(ar, "visibility", "root", &service.SessionState{})
	block.SetSize(60, 20)
	now := time.Unix(100, 0)
	block.now = func() time.Time { return now }
	msg := types.ToolCallMessage("root", tools.ToolCall{ID: "call", Function: tools.FunctionCall{Name: "read_file"}}, tools.Tool{Name: "read_file"}, types.ToolStatusRunning)
	block.AddToolCall(msg)
	require.Equal(t, int32(1), ar.ActiveCount())
	block.StopAnimation()
	require.False(t, ar.HasActive())
	block.ResumeAnimation()
	require.Equal(t, int32(1), ar.ActiveCount())
	block.ResumeAnimation()
	require.Equal(t, int32(1), ar.ActiveCount())

	// Stop the running view before replacing it, as a hidden page does.
	block.StopAnimation()
	block.UpdateToolResult("call", "done", types.ToolStatusCompleted, nil)
	deadline := block.toolEntries[0].collapsedVisibleUntil
	require.True(t, block.animationSub.IsActive())
	block.StopAnimation()
	now = deadline.Add(-completedToolFadeDuration / 2)
	block.ResumeAnimation()
	require.Equal(t, deadline, block.toolEntries[0].collapsedVisibleUntil)
	require.InDelta(t, 0.5, block.toolEntries[0].fadeProgress, 0.001)
	require.Equal(t, int32(1), ar.ActiveCount())
	block.StopAnimation()
	now = deadline.Add(time.Second)
	block.ResumeAnimation()
	require.InDelta(t, 1.0, block.toolEntries[0].fadeProgress, 1e-12)
	require.False(t, ar.HasActive(), "expired hidden fade must remain quiescent")
}

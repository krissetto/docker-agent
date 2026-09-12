package sidebar

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/docker/docker-agent/pkg/subagent"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/service"
)

func TestSubagentStartingIsStaticPending(t *testing.T) {
	t.Parallel()
	m := New(animation.NewRuntime(), t.Context(), &service.SessionState{}).(*model)

	assert.False(t, isActiveSubagentState(subagent.NodeStarting))
	assert.True(t, isActiveSubagentState(subagent.NodeRunning))
	assert.Contains(t, m.subagentGlyph(subagent.Node{Agent: "worker", State: subagent.NodeStarting}), "·")
}

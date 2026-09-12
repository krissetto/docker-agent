package subagenttool

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/docker/docker-agent/pkg/subagent"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/components/spinner"
	"github.com/docker/docker-agent/pkg/tui/styles"
	"github.com/docker/docker-agent/pkg/tui/subagentindex"
	"github.com/docker/docker-agent/pkg/tui/types"
)

func testMessage(tool, args, result string, status types.ToolStatus) *types.Message {
	msg := &types.Message{Type: types.MessageTypeToolCall, ToolStatus: status}
	msg.ToolCall.Function.Name = tool
	msg.ToolCall.Function.Arguments = args
	if result != "" {
		msg.ToolResult = &tools.ToolCallResult{Output: result}
	}
	return msg
}

func testSpinner() spinner.Spinner {
	return spinner.New(animation.NewRuntime(), spinner.ModeSpinnerOnly, styles.NoStyle)
}

func TestRenderReadShowsAttributionAndHidesResult(t *testing.T) {
	t.Parallel()

	result := "Subagent \"worker\" (a1b2c) — completed:\n\nthe secret transcript body"
	msg := testMessage("read_subagent", `{"subagent_id":"a1b2c"}`, result, types.ToolStatusCompleted)

	out := renderRead(msg, testSpinner(), nil, 80, 0, nil)

	assert.Contains(t, out, "Inspecting")
	assert.Contains(t, out, "worker")
	assert.Contains(t, out, "(a1b2c)")
	assert.NotContains(t, out, "secret transcript", "tool results must never be rendered")
	assert.NotContains(t, out, "completed")
}

// A restored transcript carries the result text in msg.Content (not
// ToolResult) and the live index is empty after a process restart: the name
// must still resolve from the stamped attribution.
func TestRenderReadRestoredTranscriptAttribution(t *testing.T) {
	t.Parallel()

	msg := testMessage("read_subagent", `{"subagent_id":"e97fa"}`, "", types.ToolStatusCompleted)
	msg.Content = "Subagent \"planner\" (e97fa) — completed:\n\nthe transcript body"

	out := renderRead(msg, testSpinner(), nil, 80, 0, nil)
	assert.Contains(t, out, "Inspecting")
	assert.Contains(t, out, "planner")
	assert.Contains(t, out, "(e97fa)")
	assert.NotContains(t, out, "transcript body")
}

func TestRenderReadResolvesNameFromLiveIndexWhileRunning(t *testing.T) {
	t.Parallel()

	index := subagentindex.New()
	index.Reset(subagent.Snapshot{Nodes: []subagent.NodeSnapshot{
		{Node: subagent.Node{ID: "beef1", Agent: "researcher", State: subagent.NodeRunning}},
	}})

	msg := testMessage("read_subagent", `{"subagent_id":"beef1"}`, "", types.ToolStatusRunning)
	out := renderRead(msg, testSpinner(), nil, 80, 0, index.Name)

	assert.Contains(t, out, "Inspecting")
	assert.Contains(t, out, "researcher", "name resolved from the live swarm index")
	assert.Contains(t, out, "(beef1)")
}

func TestRenderSpawn(t *testing.T) {
	t.Parallel()

	// Running: name from args, no id yet.
	running := testMessage("spawn_subagent", `{"agent":"coder","task":"do it"}`, "", types.ToolStatusRunning)
	out := renderSpawn(running, testSpinner(), nil, 80, 0, nil)
	assert.Contains(t, out, "Spawning")
	assert.Contains(t, out, "coder")
	assert.NotContains(t, out, "do it", "task must not be rendered")

	// Completed: attribution from the result, result body hidden.
	result := `Spawned subagent "coder" (a1b2c). It is running concurrently; do not poll.`
	done := testMessage("spawn_subagent", `{"agent":"coder","task":"do it"}`, result, types.ToolStatusCompleted)
	out = renderSpawn(done, testSpinner(), nil, 80, 0, nil)
	assert.Contains(t, out, "Spawned")
	assert.Contains(t, out, "coder")
	assert.Contains(t, out, "(a1b2c)")
	assert.NotContains(t, out, "do not poll")
}

func TestRenderStop(t *testing.T) {
	t.Parallel()

	result := `Stopped subagent "worker" (a1b2c).`
	msg := testMessage("stop_subagent", `{"subagent_id":"a1b2c"}`, result, types.ToolStatusCompleted)
	out := renderStop(msg, testSpinner(), nil, 80, 0, nil)
	assert.Contains(t, out, "Stopped")
	assert.Contains(t, out, "worker")
	assert.Contains(t, out, "(a1b2c)")
}

func TestRenderSend(t *testing.T) {
	t.Parallel()

	// To parent.
	parent := testMessage("send_message", `{"to":"parent","message":"hi"}`, "Message delivered to parent.", types.ToolStatusCompleted)
	out := renderSend(parent, testSpinner(), nil, 80, 0, nil)
	assert.Contains(t, out, "Messaged parent")
	assert.NotContains(t, out, "hi")

	// To a subagent: attribution from the result.
	result := `Message delivered to subagent "worker" (a1b2c).`
	child := testMessage("send_message", `{"to":"a1b2c","message":"keep going"}`, result, types.ToolStatusCompleted)
	out = renderSend(child, testSpinner(), nil, 80, 0, nil)
	assert.Contains(t, out, "Messaged")
	assert.Contains(t, out, "worker")
	assert.Contains(t, out, "(a1b2c)")
	assert.NotContains(t, out, "keep going", "message body must not be rendered")
}

func TestNodeIDFor(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		msg    *types.Message
		wantID string
		wantOK bool
	}{
		{
			name:   "spawn resolves from stamped result",
			msg:    testMessage(subagent.ToolSpawnSubagent, `{"agent":"coder","task":"t"}`, `Spawned subagent "coder" (9ca01).`, types.ToolStatusCompleted),
			wantID: "9ca01", wantOK: true,
		},
		{
			name:   "spawn without result has no id yet",
			msg:    testMessage(subagent.ToolSpawnSubagent, `{"agent":"coder","task":"t"}`, "", types.ToolStatusRunning),
			wantOK: false,
		},
		{
			name:   "send resolves from args",
			msg:    testMessage(subagent.ToolSendMessage, `{"to":"77c88","message":"hi"}`, "", types.ToolStatusRunning),
			wantID: "77c88", wantOK: true,
		},
		{
			name:   "send to parent is not attachable",
			msg:    testMessage(subagent.ToolSendMessage, `{"to":"parent","message":"hi"}`, "", types.ToolStatusCompleted),
			wantOK: false,
		},
		{
			name:   "read resolves from args",
			msg:    testMessage(subagent.ToolReadSubagent, `{"subagent_id":"77c88"}`, "", types.ToolStatusCompleted),
			wantID: "77c88", wantOK: true,
		},
		{
			name:   "stop resolves from args",
			msg:    testMessage(subagent.ToolStopSubagent, `{"subagent_id":"77c88"}`, "", types.ToolStatusCompleted),
			wantID: "77c88", wantOK: true,
		},
		{
			name:   "other tools are ignored",
			msg:    testMessage("shell", `{"cmd":"ls"}`, "", types.ToolStatusCompleted),
			wantOK: false,
		},
		{
			name:   "non-tool messages are ignored",
			msg:    &types.Message{Type: types.MessageTypeUser},
			wantOK: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			id, ok := NodeIDFor(tt.msg)
			assert.Equal(t, tt.wantOK, ok)
			if tt.wantOK {
				assert.Equal(t, subagent.NodeID(tt.wantID), id)
			}
		})
	}
}

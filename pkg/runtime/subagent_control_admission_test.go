package runtime

import (
	"context"
	"testing"
	"time"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/runtime/toolexec"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/subagent"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/stretchr/testify/require"
)

func TestDelegationBypassesUnrelatedToolPermit(t *testing.T) {
	for _, name := range []string{subagent.ToolStopSubagent, subagent.ToolSpawnSubagent, subagent.ToolSendMessage, subagent.ToolReadSubagent} {
		t.Run(name, func(t *testing.T) {
			r := admissionRuntime(2, 1)
			release, err := r.acquireTool(t.Context(), "unrelated-root", "fake-blocking-tool")
			require.NoError(t, err)
			defer release()
			emitted := make(chan struct{})
			entered := make(chan struct{})
			done := make(chan struct{})
			sess := session.New()
			sess.ToolsApproved = true
			a := agent.New("root", "fake")
			d := &toolexec.Dispatcher{
				AgentFor:    func(*session.Session) *agent.Agent { return a },
				AcquireTool: r.acquireTool,
				Handlers: map[string]toolexec.ToolHandler{
					name: func(context.Context, *session.Session, tools.ToolCall, tools.Runtime) (*tools.ToolCallResult, error) {
						close(entered)
						return tools.ResultSuccess("fake accepted"), nil
					},
				},
			}
			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
			defer cancel()
			events := &sinkEmitter{events: EventSinkFunc(func(e Event) {
				if _, ok := e.(*ToolCallEvent); ok {
					close(emitted)
				}
			})}
			start := time.Now()
			go func() {
				defer close(done)
				d.Process(ctx, sess, []tools.ToolCall{{ID: "diagnostic", Function: tools.FunctionCall{Name: name, Arguments: `{}`}}}, []tools.Tool{{Name: name}}, events)
			}()
			select {
			case <-emitted:
			case <-ctx.Done():
				t.Fatal("tool-start event never emitted")
			}
			select {
			case <-entered:
			case <-ctx.Done():
				t.Fatal("coordination handler waited for unrelated tool permit")
			}
			release()
			select {
			case <-done:
			case <-ctx.Done():
				t.Fatal("handler did not finish after unrelated permit released")
			}
			select {
			case <-entered:
			default:
				t.Fatal("handler was not entered")
			}
			t.Logf("coordination handler entered while unrelated permit remained held (%v total)", time.Since(start))
		})
	}
}

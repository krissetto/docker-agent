package app

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
)

type inspectorSession struct {
	projectionSession

	read func(context.Context, string) (runtime.SessionAgentConfig, error)
}

func (h *inspectorSession) SessionAgentConfig(ctx context.Context, name string) (runtime.SessionAgentConfig, error) {
	return h.read(ctx, name)
}

func TestAppAgentConfigInfoBoundAndStale(t *testing.T) {
	h := &inspectorSession{projectionSession: projectionSession{id: "bound"}}
	a := &App{currentState: sessionState{handle: h}}
	h.read = func(context.Context, string) (runtime.SessionAgentConfig, error) {
		return runtime.SessionAgentConfig{SessionID: "bound", AgentName: "worker", Info: runtime.AgentConfigInfo{MaxIterations: 9}}, nil
	}
	require.Equal(t, 9, a.AgentConfigInfo(t.Context(), "worker").MaxIterations)
	require.Empty(t, a.AgentConfigInfo(t.Context(), "foreign"))
	h.read = func(context.Context, string) (runtime.SessionAgentConfig, error) {
		a.currentState.handle = &projectionSession{id: "replacement"}
		return runtime.SessionAgentConfig{SessionID: "bound", AgentName: "worker", Info: runtime.AgentConfigInfo{MaxIterations: 9}}, nil
	}
	require.Empty(t, a.AgentConfigInfo(t.Context(), "worker"), "discard result after session replacement")
}

type exportInfoSession struct {
	projectionSession

	description string
	infoAgent   string
}

func (*exportInfoSession) AgentName() string { return "worker" }
func (h *exportInfoSession) SessionAgentInfo(context.Context) (runtime.SessionAgentInfo, error) {
	return runtime.SessionAgentInfo{Agent: &runtime.AgentInfoEvent{AgentName: h.infoAgent, Description: h.description}}, nil
}

func TestExportHTMLUsesBoundAgentDescription(t *testing.T) {
	for _, name := range []string{"worker", "foreign"} {
		t.Run(name, func(t *testing.T) {
			h := &exportInfoSession{projectionSession: projectionSession{id: "child"}, description: "BOUND-DESCRIPTION", infoAgent: name}
			sess := session.New(session.WithID("child"))
			sess.AddMessage(session.UserMessage("hello"))
			a := &App{currentState: sessionState{handle: h, session: sess}}
			path, err := a.ExportHTML(t.Context(), filepath.Join(t.TempDir(), "session.html"))
			require.NoError(t, err)
			data, err := os.ReadFile(path)
			require.NoError(t, err)
			if name == "worker" {
				require.Contains(t, string(data), "BOUND-DESCRIPTION")
			} else {
				require.NotContains(t, string(data), "BOUND-DESCRIPTION")
			}
		})
	}
}

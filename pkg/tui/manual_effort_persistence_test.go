package tui

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/effort"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/model/provider"
	"github.com/docker/docker-agent/pkg/model/provider/openai"
	"github.com/docker/docker-agent/pkg/model/provider/options"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/tui/dialog"
	"github.com/docker/docker-agent/pkg/tui/messages"
)

func TestManualEffortPickerPersistsThroughTurnAndRestore(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	var mu sync.Mutex
	var sent []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Reasoning struct {
				Effort string `json:"effort"`
			} `json:"reasoning"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		mu.Lock()
		sent = append(sent, request.Reasoning.Effort)
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"ok\"}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"done\",\"output\":[]}}\n\ndata: [DONE]\n\n")
	}))
	defer server.Close()
	cfg := latest.ModelConfig{Provider: "openai", Model: "gpt-6-sol", BaseURL: server.URL, ThinkingBudget: &latest.ThinkingBudget{Effort: "low"}, ProviderOpts: map[string]any{"api_type": "openai_responses"}}
	env := environment.NewMapEnvProvider(map[string]string{"OPENAI_API_KEY": "offline-test"})
	factory := func(ctx context.Context, cfg *latest.ModelConfig, env environment.Provider, opts ...options.Opt) (provider.Provider, error) {
		return openai.NewClient(ctx, cfg, env, opts...)
	}
	registry := provider.NewRegistry(map[string]provider.Factory{"openai": factory, "openai_responses": factory})
	store := session.NewInMemorySessionStore()
	newRuntime := func() *runtime.LocalRuntime {
		p, err := registry.New(ctx, &cfg, env)
		require.NoError(t, err)
		rt, err := runtime.NewLocalRuntime(ctx, team.New(team.WithAgents(agent.New("root", "offline", agent.WithModel(p)))), runtime.WithSessionCompaction(false), runtime.WithSessionStore(store), runtime.WithModelStore(attachToolModelStore{}), runtime.WithModelSwitcherConfig(&runtime.ModelSwitcherConfig{ProviderRegistry: registry, EnvProvider: env}))
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, rt.Close()) })
		return rt
	}
	rt := newRuntime()
	sess := session.New(session.WithAgentName("root"), session.WithTitle("offline"))
	application := app.New(ctx, rt, sess, runtime.SessionBinding{AgentName: "root"}, app.WithRuntimeServices(rt))
	root := newSidebarProgramRoot(t, application)
	handle := application.SessionHandle()
	require.NotNil(t, handle)
	// Use the picker's real Enter command, not a direct controller mutation.
	picker := dialog.NewEffortPickerDialog([]effort.Level{effort.Low, effort.High}, effort.Low)
	picker.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	_, cmd := picker.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	selected, ok := firstOfType[messages.SetThinkingLevelMsg](collectMsgs(cmd))
	require.True(t, ok)
	root.Update(selected)
	require.Equal(t, effort.High, handle.CurrentThinkingLevel(ctx))
	check := func(h runtime.SessionHandle) {
		t.Helper()
		obs, err := h.Observe(ctx, runtime.ObserveOptions{Buffer: 128})
		require.NoError(t, err)
		defer obs.Cancel()
		roster := obs.Primary().Presentation[1].(*runtime.TeamInfoEvent)
		require.Equal(t, "high", roster.AvailableAgents[0].Thinking)
		require.Equal(t, "high", roster.AvailableAgents[0].ThinkingLevel)
		root.Update(roster)
		require.Equal(t, "high", root.sessionState.GetCurrentAgent().Thinking)
		require.Equal(t, "high", root.sessionState.GetCurrentAgent().ThinkingControl().Level)
		turn, err := h.Submit(ctx, runtime.TurnInput{Content: "go"})
		require.NoError(t, err)
		for e := range obs.Events {
			if e.TurnID != turn.TurnID {
				continue
			}
			if info, ok := e.Event.(*runtime.TeamInfoEvent); ok {
				require.Equal(t, "high", info.AvailableAgents[0].Thinking)
				require.Equal(t, "high", info.AvailableAgents[0].ThinkingLevel)
			}
			if _, ok := e.Event.(*runtime.StreamStoppedEvent); ok {
				break
			}
		}
		require.NoError(t, h.AwaitTurn(ctx, turn.TurnID))
	}
	check(handle)
	loaded, err := store.GetSession(ctx, sess.ID)
	require.NoError(t, err)
	require.NoError(t, rt.Close())
	restored, err := newRuntime().CreateSession(ctx, loaded, runtime.SessionBinding{AgentName: "root"})
	require.NoError(t, err)
	check(restored)
	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, []string{"high", "high"}, sent)
}

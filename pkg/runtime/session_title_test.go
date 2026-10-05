package runtime

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/hooks"
	"github.com/docker/docker-agent/pkg/model/provider/base"
	"github.com/docker/docker-agent/pkg/modelsdev"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/sessiontitle"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/tools"
)

type ownedTitleProvider struct {
	entered chan struct{}
	release chan struct{}
	calls   atomic.Int32
	failure error
}

func (*ownedTitleProvider) ID() modelsdev.ID        { return modelsdev.ParseIDOrZero("test/title") }
func (*ownedTitleProvider) BaseConfig() base.Config { return base.Config{} }
func (*ownedTitleProvider) MaxTokens() int          { return 128 }
func (p *ownedTitleProvider) CreateChatCompletionStream(ctx context.Context, _ []chat.Message, _ []tools.Tool) (chat.MessageStream, error) {
	p.calls.Add(1)
	if p.entered != nil {
		close(p.entered)
	}
	if p.release != nil {
		select {
		case <-p.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if p.failure != nil {
		return nil, p.failure
	}
	return newStreamBuilder().AddContent("Owned title").Build(), nil
}

func titleOwnerFixture(t *testing.T, p *ownedTitleProvider, opts ...Opt) *sessionHandle {
	t.Helper()
	root := agent.New("root", "", agent.WithModel(&mockProvider{id: "test/main", stream: newStreamBuilder().AddContent("Answer").Build()}), agent.WithTitleModel(p))
	worker := agent.New("worker", "", agent.WithModel(&mockProvider{id: "test/worker"}))
	opts = append(opts, WithModelStore(mockModelStore{}), WithSessionStore(session.NewInMemorySessionStore()))
	r, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(root, worker)), opts...)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, r.Close()) })
	h, err := r.CreateSession(t.Context(), session.New(session.WithID(t.Name()), session.WithAgentName("root")), SessionBinding{AgentName: "root"})
	require.NoError(t, err)
	return h.(*sessionHandle)
}

func titleStatus(t *testing.T, h *sessionHandle) string {
	t.Helper()
	observation, err := h.Observe(t.Context(), ObserveOptions{})
	require.NoError(t, err)
	defer observation.Cancel()
	return observation.Primary().TitleStatus
}

func TestSessionTitleOwnerTerminalSuccessFailureAndPreturnMetadata(t *testing.T) {
	for _, failure := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "failure"}[failure], func(t *testing.T) {
			p := &ownedTitleProvider{entered: make(chan struct{}), release: make(chan struct{})}
			if failure {
				p.failure = errors.New("offline")
			}
			h := titleOwnerFixture(t, p)
			observation, err := h.Observe(t.Context(), ObserveOptions{})
			require.NoError(t, err)
			defer observation.Cancel()
			require.Len(t, observation.Primary().Presentation, 2)
			info := observation.Primary().Presentation[0].(*AgentInfoEvent)
			require.Equal(t, "root", info.AgentName)
			require.Equal(t, "test/main", info.Model)
			require.Zero(t, p.calls.Load())
			// Dedicated custom generator avoids fallback so failure is deterministic.
			require.NoError(t, h.GenerateSessionTitle(t.Context(), sessiontitle.New(p), []string{"hello"}, false))
			<-p.entered
			require.Equal(t, "started", titleStatus(t, h))
			close(p.release)
			h.driver.wg.Wait()
			want := "completed"
			if failure {
				want = "failed"
			}
			require.Equal(t, want, titleStatus(t, h))
			snapshot, err := h.Snapshot(t.Context())
			require.NoError(t, err)
			if failure {
				require.Empty(t, snapshot.TitleSnapshot())
			} else {
				require.Equal(t, "Owned title", snapshot.TitleSnapshot())
			}
			require.Equal(t, int32(1), p.calls.Load())
			require.NoError(t, h.GenerateSessionTitle(t.Context(), sessiontitle.New(p), []string{"again"}, false))
			require.Equal(t, int32(1), p.calls.Load(), "automatic request cannot retry or replace an existing operation")
		})
	}
}

func TestSessionTitleOwnerFencesManualRestoreStopAndCanceledContext(t *testing.T) {
	for _, action := range []string{"manual", "restore", "stop", "caller-cancel"} {
		t.Run(action, func(t *testing.T) {
			p := &ownedTitleProvider{entered: make(chan struct{}), release: make(chan struct{})}
			h := titleOwnerFixture(t, p)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			require.NoError(t, h.GenerateSessionTitle(ctx, sessiontitle.New(p), []string{"hello"}, false))
			<-p.entered
			switch action {
			case "manual":
				require.NoError(t, h.UpdateTitle(t.Context(), "Manual"))
			case "restore":
				snapshot, err := h.Snapshot(t.Context())
				require.NoError(t, err)
				snapshot.SetTitle("Restored")
				require.True(t, h.ReplaceSettledSession(snapshot))
			case "stop":
				h.driver.StopAll()
			case "caller-cancel":
				cancel()
			}
			close(p.release)
			h.driver.wg.Wait()
			snapshot, err := h.Snapshot(t.Context())
			require.NoError(t, err)
			switch action {
			case "manual":
				require.Equal(t, "Manual", snapshot.TitleSnapshot())
			case "restore":
				require.Equal(t, "Restored", snapshot.TitleSnapshot())
			case "stop":
				require.Empty(t, snapshot.TitleSnapshot())
			case "caller-cancel":
				require.Equal(t, "Owned title", snapshot.TitleSnapshot())
			}
		})
	}
}

func TestSessionTitleOwnerOutboundPolicyOriginAndFailClosed(t *testing.T) {
	p := &ownedTitleProvider{}
	var seen *hooks.Input
	h := titleOwnerFixture(t, p, WithMessagePolicy("title-origin", func(_ context.Context, in *hooks.Input, _ []chat.Message) ([]chat.Message, error) {
		seen = in
		return nil, errors.New("denied")
	}))
	require.NoError(t, h.GenerateSessionTitle(t.Context(), sessiontitle.New(p), []string{"secret"}, false))
	h.driver.wg.Wait()
	require.NotNil(t, seen)
	require.Equal(t, h.ID(), seen.SessionID)
	require.Equal(t, h.ID(), seen.RootSessionID)
	require.Equal(t, "root", seen.AgentName)
	require.Equal(t, "title", seen.CallPurpose)
	require.Zero(t, p.calls.Load())
	require.Equal(t, "failed", titleStatus(t, h))
}

func TestSessionTitleExplicitTurnIntentOnly(t *testing.T) {
	for _, requested := range []bool{false, true} {
		t.Run(map[bool]string{false: "opt-out", true: "opt-in"}[requested], func(t *testing.T) {
			p := &ownedTitleProvider{}
			h := titleOwnerFixture(t, p)
			submitted, err := h.Submit(t.Context(), TurnInput{Content: "hello", GenerateTitle: requested, TitleGenerator: sessiontitle.New(p)})
			require.NoError(t, err)
			require.NoError(t, h.AwaitTurn(t.Context(), submitted.TurnID))
			h.driver.wg.Wait()
			want := int32(0)
			if requested {
				want = 1
			}
			require.Equal(t, want, p.calls.Load())
		})
	}
}

func TestSessionTitleWorkerCancelTurnIsTerminal(t *testing.T) {
	p := &ownedTitleProvider{entered: make(chan struct{}), release: make(chan struct{})}
	h := titleOwnerFixture(t, p)
	require.NoError(t, h.GenerateSessionTitle(t.Context(), sessiontitle.New(p), []string{"hello"}, false))
	<-p.entered
	require.NoError(t, h.driver.ownerCall(t.Context(), func() error {
		h.driver.phase = sessionRunning
		h.driver.activeRequestID = "turn"
		h.driver.cancel = func() {}
		return nil
	}))
	outcome, err := h.driver.cancelTurn(t.Context(), "turn")
	require.NoError(t, err)
	require.Equal(t, CancelAccepted, outcome)
	require.Eventually(t, func() bool { return titleStatus(t, h) == "canceled" }, time.Second, time.Millisecond)
	close(p.release)
	h.driver.wg.Wait()
	require.NoError(t, h.driver.ownerCall(t.Context(), func() error { h.driver.phase = sessionIdle; return nil }))
}

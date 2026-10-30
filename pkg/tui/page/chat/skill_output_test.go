package chat

import (
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/types"
)

func TestSameAgentContentSessionBoundaries(t *testing.T) {
	t.Parallel()

	for _, reasoning := range []bool{false, true} {
		name := "text"
		if reasoning {
			name = "reasoning"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			sess := session.New()
			p := New(animation.NewRuntime(), t.Context(), app.New(t.Context(), nil, sess, runtime.SessionBinding{}, app.WithRuntimeServices(queueTestRuntime{})), service.NewSessionState(sess)).(*chatPage)
			contentEvent := runtime.AgentChoice
			messageType := types.MessageTypeAssistant
			if reasoning {
				contentEvent = runtime.AgentChoiceReasoning
				messageType = types.MessageTypeAssistantReasoningBlock
			}
			for _, event := range []runtime.Event{
				runtime.StreamStarted(sess.ID, "root"),
				contentEvent("root", sess.ID, "parent"),
				contentEvent("root", "", " legacy chunk"),
				runtime.StreamStarted("first-skill", "root"),
				runtime.StreamStarted("second-skill", "root"),
				contentEvent("root", "first-skill", "first"),
				contentEvent("root", "first-skill", " chunk"),
				contentEvent("root", "second-skill", "second"),
				contentEvent("root", "first-skill", "first again"),
			} {
				_, _ = p.handleRuntimeEvent(event)
			}
			require.Equal(t, 4, p.messages.MessageTypeCount(messageType))
		})
	}
}

func TestSameSessionNewStreamStartsNewMessage(t *testing.T) {
	t.Parallel()

	sess := session.New()
	p := New(animation.NewRuntime(), t.Context(), app.New(t.Context(), nil, sess, runtime.SessionBinding{}, app.WithRuntimeServices(queueTestRuntime{})), service.NewSessionState(sess)).(*chatPage)
	for range 2 {
		_, _ = p.handleRuntimeEvent(runtime.StreamStarted(sess.ID, "root"))
		_, _ = p.handleRuntimeEvent(runtime.AgentChoice("root", sess.ID, "reply"))
		_, _ = p.handleRuntimeEvent(runtime.StreamStopped(sess.ID, "root", "normal"))
	}
	require.Equal(t, 2, p.messages.MessageTypeCount(types.MessageTypeAssistant))
}

func TestSameAgentMediaSessionsStaySeparate(t *testing.T) {
	t.Parallel()

	p, _ := newGeneratedMediaTestPage(t, &resolverTestRuntime{})
	_, _ = p.handleRuntimeEvent(runtime.AgentChoice("root", "child", "child text"))
	_, _ = p.handleRuntimeEvent(assistantMessageAdded("child", workspaceImagePart("child.png", "child.png", "child")))
	_, _ = p.handleRuntimeEvent(assistantMessageAdded("parent", workspaceImagePart("parent.png", "parent.png", "parent")))
	_, _ = p.handleRuntimeEvent(runtime.AgentChoice("root", "parent", "parent text"))
	require.Equal(t, 2, p.messages.MessageTypeCount(types.MessageTypeAssistant))
}

func TestSiblingStreamLifecycleDoesNotSplitContent(t *testing.T) {
	t.Parallel()

	sess := session.New()
	p := New(animation.NewRuntime(), t.Context(), app.New(t.Context(), nil, sess, runtime.SessionBinding{}, app.WithRuntimeServices(queueTestRuntime{})), service.NewSessionState(sess)).(*chatPage)
	p.messages.SetSize(100, 40)
	for _, event := range []runtime.Event{
		runtime.StreamStarted(sess.ID, "root"),
		runtime.StreamStarted("first-skill", "root"),
		runtime.AgentChoice("root", "first-skill", "https://github.com/"),
		runtime.StreamStarted("second-skill", "root"),
		runtime.AgentChoice("root", "first-skill", "docker/gordon/pull/"),
		runtime.StreamStopped("second-skill", "root", "normal"),
		runtime.AgentChoice("root", "first-skill", "2287"),
	} {
		_, _ = p.handleRuntimeEvent(event)
	}
	require.Equal(t, 1, p.messages.MessageTypeCount(types.MessageTypeAssistant))
	require.Contains(t, ansi.Strip(p.messages.View()), "https://github.com/docker/gordon/pull/2287")
}

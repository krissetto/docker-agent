package chat

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/app"
	chatmsg "github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/components/workblock"
	msgtypes "github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/types"
)

func TestCompactWorkLogOptionDefaultOptOutAndLeanOverride(t *testing.T) {
	newPage := func(opts ...PageOption) *chatPage {
		sess := session.New()
		a, _ := newSessionTestApp(t, sess, nil, nil)
		p := New(animation.NewRuntime(), t.Context(), a, service.NewSessionState(sess), opts...).(*chatPage)
		t.Cleanup(func() { Cleanup(p); p.ar.Stop() })
		return p
	}
	workEnabled := func(t *testing.T, p *chatPage) bool {
		t.Helper()
		value := reflect.ValueOf(p.messages).Elem().FieldByName("workEnabled")
		require.True(t, value.IsValid())
		return value.Bool()
	}

	defaultPage := newPage()
	require.True(t, defaultPage.compactWorkLog)
	require.True(t, workEnabled(t, defaultPage), "full TUI enables compact work by default")

	optOut := newPage(WithCompactWorkLog(false))
	require.False(t, optOut.compactWorkLog)
	require.False(t, workEnabled(t, optOut))
	optOut.SetCompactWorkLog(true)
	require.True(t, workEnabled(t, optOut))
	optOut.SetCompactWorkLog(false)
	require.False(t, workEnabled(t, optOut))

	lean := newPage(WithLeanMode(), WithCompactWorkLog(true))
	require.False(t, workEnabled(t, lean), "lean mode always opts out independent of option order")
	lean.SetCompactWorkLog(true)
	require.False(t, workEnabled(t, lean), "lean mode setter cannot enable compact transcript")
}

type animationOverlay struct {
	fakeOverlay

	ticks int
}

func (o *animationOverlay) UpdateAnimation(tea.Msg) bool {
	o.ticks++
	return true
}

func TestOverlayAnimationTicksAdvanceOnlyVisibleActiveOverlay(t *testing.T) {
	p := newTestChatPage(t)
	p.SetSize(140, 40)
	overlay := &animationOverlay{}
	p.SetContentOverlay(overlay)
	before := p.shellVisualGeneration
	p.SetScrollbarsActive(true)
	p.Update(animation.TickMsg{})
	require.Equal(t, 1, overlay.ticks)
	require.Greater(t, p.shellVisualGeneration, before, "overlay visual generation follows animation without transcript relayout")

	p.SetPresentationVisible(false)
	p.Update(animation.TickMsg{})
	require.Equal(t, 1, overlay.ticks, "hidden page doesn't advance overlay animations")
	p.SetPresentationVisible(true)
	p.SetScrollbarsActive(false)
	p.Update(animation.TickMsg{})
	require.Equal(t, 1, overlay.ticks, "root-inactive page doesn't advance overlay animation")
}

func TestRootStreamWorkStatusTracksOnlyOuterLifecycle(t *testing.T) {
	sess := session.New()
	a, _ := newSessionTestApp(t, sess, nil, nil)
	p := New(animation.NewRuntime(), t.Context(), a, service.NewSessionState(sess)).(*chatPage)
	t.Cleanup(func() { Cleanup(p); p.ar.Stop() })
	p.handleRuntimeEvent(runtime.StreamStarted(sess.ID, "root"))
	active, elapsed := workStatus(t, p)
	require.True(t, active)
	require.Zero(t, elapsed)

	p.handleRuntimeEvent(runtime.StreamStarted("child", "subagent"))
	active, _ = workStatus(t, p)
	require.True(t, active, "nested start leaves the root work state active")
	p.handleRuntimeEvent(runtime.StreamStopped("child", "subagent", "normal"))
	active, _ = workStatus(t, p)
	require.True(t, active, "nested stop does not settle the root work header")

	p.streamStartTime = time.Now().Add(-3 * time.Second)
	p.handleRuntimeEvent(runtime.StreamStopped(sess.ID, "root", "normal"))
	active, elapsed = workStatus(t, p)
	require.False(t, active)
	require.Zero(t, elapsed, "a settled duration is never inferred from the page clock; it comes from turn timing")
}

func TestResumedRootWorkUsesWorkingHeaderWithoutDuplicatePendingSpinner(t *testing.T) {
	sess := session.New(session.WithID("work-resume-spinner"))
	a, _ := newSessionTestApp(t, sess, nil, nil)
	ar := animation.NewRuntime()
	p := New(ar, t.Context(), a, service.NewSessionState(sess)).(*chatPage)
	t.Cleanup(func() { Cleanup(p); ar.Stop() })
	p.SetSize(100, 30)

	p.handleRuntimeEvent(&runtime.UserMessageEvent{SessionID: sess.ID, Message: "first user request", SessionPosition: -1})
	p.handleRuntimeEvent(runtime.StreamStarted(sess.ID, "root"))
	p.handleRuntimeEvent(runtime.AgentChoiceReasoning("root", sess.ID, "Checking the task before running the tool."))
	call := tools.ToolCall{ID: "work-resume-call", Function: tools.FunctionCall{Name: "shell", Arguments: `{"cmd":"true"}`}}
	p.handleRuntimeEvent(runtime.PartialToolCall(call, tools.Tool{Name: "shell"}, "root"))
	tool := runtime.ToolCallResponse(call.ID, tools.Tool{Name: "shell"}, &tools.ToolCallResult{Output: "verified"}, "verified", "root").(*runtime.ToolCallResponseEvent)
	p.handleRuntimeEvent(tool)
	p.handleRuntimeEvent(runtime.StreamStopped(sess.ID, "root", "normal"))
	require.Zero(t, p.messages.MessageTypeCount(types.MessageTypeSpinner), "settled Work leaves no pending spinner")
	p.handleRuntimeEvent(&runtime.UserMessageEvent{SessionID: sess.ID, Message: "subagent delivered an incoming reply", SessionPosition: -1, InputOrigin: session.InputOriginAgent, InputMode: "steer", SenderID: "child-session", SenderName: "worker"})

	p.handleRuntimeEvent(runtime.StreamStarted(sess.ID, "root"))
	if owner, ok := p.messages.(interface {
		WorkBlockAtSource(source int) *workblock.Model
	}); ok {
		if block := owner.WorkBlockAtSource(1); block != nil {
			block.SetExpandedImmediately(true)
			p.messages.RenderedContentHeight()
		}
	}
	view := ansi.Strip(p.messages.View())
	require.Contains(t, view, "Working", "the active state returns to the existing Work header")
	require.Contains(t, view, "From worker", "the incoming reply retains its sender attribution")
	require.Zero(t, p.messages.MessageTypeCount(types.MessageTypeSpinner), "resuming existing Work does not add a second spinner row")
	active, _ := workStatus(t, p)
	require.True(t, active)

	p.handleRuntimeEvent(&runtime.UserMessageEvent{SessionID: sess.ID, Message: "next human turn", SessionPosition: -1})
	p.handleRuntimeEvent(runtime.StreamStopped(sess.ID, "root", "normal"))
	p.handleRuntimeEvent(runtime.StreamStarted(sess.ID, "root"))
	require.Equal(t, 1, p.messages.MessageTypeCount(types.MessageTypeSpinner), "a new human turn still gets its initial response spinner")
}

func workStatus(t *testing.T, p *chatPage) (bool, time.Duration) {
	t.Helper()
	value := reflect.ValueOf(p.messages).Elem()
	active, elapsed := value.FieldByName("workActive"), value.FieldByName("workElapsed")
	require.True(t, active.IsValid() && elapsed.IsValid())
	return active.Bool(), time.Duration(elapsed.Int())
}

func TestDefaultGroupedWorkPreservesHumanBoundaryAndExpandsSourceChildren(t *testing.T) {
	sess := session.New()
	a, _ := newSessionTestApp(t, sess, nil, nil)
	ar := animation.NewRuntimeWithScheduler(&referenceHoverScheduler{now: time.Unix(1, 0)})
	p := New(ar, t.Context(), a, service.NewSessionState(sess)).(*chatPage)
	t.Cleanup(func() { Cleanup(p); ar.Stop() })
	p.SetSize(100, 40)
	p.messages.SetPosition(0, 0)
	p.messages.AddUserMessage("human request stays visible")
	p.messages.AppendReasoning("root", "collapsed reasoning details")
	frame := ansi.Strip(p.messages.View())
	require.Contains(t, frame, "human request stays visible", "actual user input remains a top-level boundary")
	require.Contains(t, frame, "Work", "full TUI groups source-owned work by default")
	require.Contains(t, frame, "collapsed reasoning details", "settled collapsed Work retains displayable reasoning")
	require.NotContains(t, frame, "Thinking", "nested thinking headers stay hidden inside collapsed Work")

	owner, ok := p.messages.(interface {
		WorkBlockAtSource(source int) *workblock.Model
	})
	require.True(t, ok)
	block := owner.WorkBlockAtSource(1)
	require.NotNil(t, block)
	for line, text := range strings.Split(frame, "\n") {
		if strings.Contains(text, "Work") {
			p.messages.Update(tea.MouseClickMsg{X: 1, Y: line, Button: tea.MouseLeft})
			break
		}
	}
	for block.NeedsTick() {
		cmd := ar.Continue()
		require.NotNil(t, cmd)
		tick, accepted := ar.Accept(cmd().(animation.TickMsg))
		require.True(t, accepted)
		p.Update(tick)
	}
	frame = ansi.Strip(p.messages.View())
	require.Contains(t, frame, "collapsed reasoning details", "expansion exposes source chronology through the existing child renderer")
	require.Equal(t, 1, p.messages.MessageTypeCount(types.MessageTypeUser))
}

func TestScrollbarVisibilityAndPointerRespectPageOcclusion(t *testing.T) {
	p := newTestChatPage(t)
	p.SetSize(140, 40)
	messageState := func() (runtimeBound, active, pointerKnown bool) {
		m := reflect.ValueOf(p.messages).Elem()
		runtimeBound = m.FieldByName("workScrollbarBound").Bool()
		scroll := m.FieldByName("scrollview").Elem()
		visibility := scroll.FieldByName("visibility")
		active, pointerKnown = visibility.FieldByName("active").Bool(), visibility.FieldByName("pointerKnown").Bool()
		return
	}
	bound, active, _ := messageState()
	require.True(t, bound, "full TUI binds transcript scrollbar to the page runtime on initialization")
	require.True(t, active)
	p.SetScrollbarPointer(140, 5)
	_, _, pointer := messageState()
	require.True(t, pointer)

	p.SetContentOverlay(&fakeOverlay{})
	_, active, pointer = messageState()
	require.False(t, active, "overlay occludes transcript scrollbar")
	require.False(t, pointer, "overlay clears transcript pointer proximity")
	p.SetScrollbarPointer(140, 5)
	_, _, pointer = messageState()
	require.False(t, pointer, "hidden transcript does not receive new pointer proximity")

	p.SetContentOverlay(nil)
	_, active, _ = messageState()
	require.True(t, active, "restoring transcript reactivates its scrollbar")
	p.SetScrollbarsActive(false)
	_, active, _ = messageState()
	require.False(t, active, "root page focus suspends scrollbars")
	p.SetScrollbarPointer(140, 5)
	_, _, pointer = messageState()
	require.False(t, pointer, "root-inactive page clears pointer state")
	p.SetScrollbarsActive(true)
	_, active, _ = messageState()
	require.True(t, active)

	lean := newTestChatPage(t)
	lean.leanMode = true
	lean.SetScrollbarsActive(true)
	lean.applyScrollbarVisibility()
	m := reflect.ValueOf(lean.messages).Elem()
	scroll := m.FieldByName("scrollview").Elem()
	visibility := scroll.FieldByName("visibility")
	require.False(t, visibility.FieldByName("active").Bool(), "lean mode remains static and does not opt into animated scrollbars")
	p.ar.Stop()
	lean.ar.Stop()
}

func TestGroupedPromotedHumanInputRemainsVisibleAndStartsNextExchange(t *testing.T) {
	sess := session.New()
	a, _ := newSessionTestApp(t, sess, nil, nil)
	p := New(animation.NewRuntime(), t.Context(), a, service.NewSessionState(sess)).(*chatPage)
	t.Cleanup(func() { Cleanup(p); p.ar.Stop() })
	p.SetSize(100, 40)
	p.handleRuntimeEvent(&runtime.PendingUserMessageAcceptedEvent{
		SessionID: sess.ID, TurnID: "human-turn", Message: "promoted human stays visible",
		InputOrigin: session.InputOriginUser, SessionPosition: 0,
	})
	p.handleRuntimeEvent(&runtime.PendingUserMessagePromotedEvent{
		SessionID: sess.ID, TurnID: "human-turn", Message: "promoted human stays visible",
		InputOrigin: session.InputOriginUser, SessionPosition: 0,
	})
	frame := ansi.Strip(p.messages.View())
	require.Contains(t, frame, "promoted human stays visible")
	require.Equal(t, 1, p.messages.MessageTypeCount(types.MessageTypeUser))
}

func TestResumedWorkFromSnapshotPreservesSpinnerModesAndCancellation(t *testing.T) {
	for _, route := range []string{"replay", "attached", "projected"} {
		for _, compact := range []bool{false, true} {
			for _, origin := range []session.InputOrigin{session.InputOriginAgent, session.InputOriginRuntime} {
				t.Run(fmt.Sprintf("%s/compact=%t/origin=%s", route, compact, origin), func(t *testing.T) {
					sess := session.New(session.WithID("resumed-snapshot"))
					sess.AddMessage(session.UserMessage("existing request"))
					sess.AddMessage(session.NewAgentMessage("root", &chatmsg.Message{Role: chatmsg.MessageRoleAssistant, ReasoningContent: "existing work"}))
					var opts []app.Opt
					if route == "attached" {
						opts = append(opts, app.WithSubagentAttach(runtime.SubagentAttachInfo{NodeID: "child", Agent: "root", Session: sess}))
					}
					a, _ := newSessionTestApp(t, sess, nil, nil, opts...)
					ar := animation.NewRuntime()
					p := New(ar, t.Context(), a, service.NewSessionState(sess), WithCompactWorkLog(compact)).(*chatPage)
					t.Cleanup(func() { Cleanup(p); ar.Stop() })
					p.SetSize(100, 30)
					p.resetProjection(runtime.SessionSnapshot{Session: sess, Status: runtime.SessionStatus{SessionID: sess.ID, State: runtime.SessionStateSettled}, TranscriptPosition: sess.ItemCount()})
					p.handleRuntimeEvent(&runtime.UserMessageEvent{SessionID: sess.ID, Message: "incoming completion", SessionPosition: -1, InputOrigin: origin, InputMode: "steer", SenderID: "child", SenderName: "worker"})
					deliver := func(event runtime.Event) {
						var projection *app.PresentationState
						if route == "projected" {
							state, _ := p.lifecycle.Apply(event)
							projection = &app.PresentationState{Lifecycle: state}
						}
						p.handleRuntimeEvent(msgtypes.SessionRuntimeEventMsg{Event: event, Projection: projection, OriginSessionID: sess.ID, Sequence: 1})
					}
					deliver(runtime.StreamStarted(sess.ID, "root"))
					active, _ := workStatus(t, p)
					if compact {
						require.True(t, active)
						require.Contains(t, ansi.Strip(p.messages.View()), "Working")
						require.Zero(t, p.messages.MessageTypeCount(types.MessageTypeSpinner))
					} else {
						require.Zero(t, p.messages.MessageTypeCount(types.MessageTypeSpinner), "existing response must not rearm pending in compact-off either")
					}
					deliver(runtime.StreamStopped(sess.ID, "root", "cancelled"))
					active, _ = workStatus(t, p)
					require.False(t, active)
					require.Zero(t, p.messages.MessageTypeCount(types.MessageTypeSpinner))
					require.NotContains(t, ansi.Strip(p.messages.View()), "Working")
					deliver(runtime.StreamStarted(sess.ID, "root"))
					if compact {
						require.Zero(t, p.messages.MessageTypeCount(types.MessageTypeSpinner), "restart after cancellation still uses the existing Work header")
						require.Contains(t, ansi.Strip(p.messages.View()), "Working")
					}
				})
			}
		}
	}
}

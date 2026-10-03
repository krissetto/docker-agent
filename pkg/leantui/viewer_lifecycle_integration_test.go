package leantui

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/model/provider/base"
	"github.com/docker/docker-agent/pkg/modelsdev"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/subagent"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/tools"
	skillstool "github.com/docker/docker-agent/pkg/tools/builtin/skills"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/service"
)

// These fixtures fake presentation services and provider I/O, not session
// ownership: all admission, topology, journals, handles and observers are real.
// No terminal, shell command, network listener, model service or disk store is
// involved. Physical terminal scrollback is global; the saved Screen/Transcript
// is the session-local history that this integration can restore and inspect.
type viewerLifecycleServices struct {
	starts atomic.Int32
}

func (*viewerLifecycleServices) CurrentAgentInfo(context.Context) runtime.CurrentAgentInfo {
	return runtime.CurrentAgentInfo{}
}

func (*viewerLifecycleServices) CurrentAgentTools(context.Context) ([]tools.Tool, error) {
	return nil, nil
}
func (*viewerLifecycleServices) CurrentAgentToolsetStatuses() []tools.ToolsetStatus { return nil }
func (*viewerLifecycleServices) RestartToolset(context.Context, string) error {
	return runtime.ErrUnsupported
}

func (s *viewerLifecycleServices) EmitStartupInfo(_ context.Context, sess *session.Session, sink runtime.EventSink) {
	s.starts.Add(1)
	sink.Emit(runtime.AgentInfo(sess.AgentName, "fixture/model", "", "", 0))
}
func (*viewerLifecycleServices) EmitAgentInfo(context.Context, runtime.EventSink) {}
func (*viewerLifecycleServices) ResetStartupInfo()                                {}
func (*viewerLifecycleServices) SessionStore() session.Store                      { return nil }
func (*viewerLifecycleServices) PermissionsInfo() *runtime.PermissionsInfo        { return nil }
func (*viewerLifecycleServices) CurrentAgentSkillsToolset() *skillstool.ToolSet   { return nil }
func (*viewerLifecycleServices) CurrentMCPPrompts(context.Context) map[string]tools.PromptInfo {
	return nil
}

func (*viewerLifecycleServices) ExecuteMCPPrompt(context.Context, string, map[string]string) (string, error) {
	return "", runtime.ErrUnsupported
}

func (*viewerLifecycleServices) UpdateSessionTitle(context.Context, *session.Session, string) error {
	return runtime.ErrUnsupported
}
func (*viewerLifecycleServices) OnToolsChanged(func(runtime.Event))    {}
func (*viewerLifecycleServices) OnBackgroundEvent(func(runtime.Event)) {}

type viewerLifecycleLocalServices struct {
	viewerLifecycleServices

	local *runtime.LocalRuntime
}

func (s *viewerLifecycleLocalServices) SubagentAttachInfo(id subagent.NodeID) (runtime.SubagentAttachInfo, bool) {
	return s.local.SubagentAttachInfo(id)
}

func (s *viewerLifecycleLocalServices) SubagentViewInfo(id subagent.NodeID) (runtime.SubagentAttachInfo, bool) {
	return s.local.SubagentViewInfo(id)
}

func (s *viewerLifecycleLocalServices) SubagentNodeForSession(id string) (subagent.NodeID, bool) {
	return s.local.SubagentNodeForSession(id)
}

type viewerLifecycleCall struct {
	messages []chat.Message
	reply    chan string
}

type viewerLifecycleProvider struct {
	calls    chan viewerLifecycleCall
	canceled atomic.Int32
}

func (*viewerLifecycleProvider) ID() modelsdev.ID        { return modelsdev.NewID("test", "viewer") }
func (*viewerLifecycleProvider) BaseConfig() base.Config { return base.Config{} }
func (p *viewerLifecycleProvider) CreateChatCompletionStream(ctx context.Context, input []chat.Message, _ []tools.Tool) (chat.MessageStream, error) {
	if p.calls == nil {
		return &viewerLifecycleStream{text: "root response"}, nil
	}
	call := viewerLifecycleCall{messages: input, reply: make(chan string, 1)}
	select {
	case p.calls <- call:
	case <-ctx.Done():
		p.canceled.Add(1)
		return nil, ctx.Err()
	}
	select {
	case text := <-call.reply:
		return &viewerLifecycleStream{text: text}, nil
	case <-ctx.Done():
		p.canceled.Add(1)
		return nil, ctx.Err()
	}
}

type viewerLifecycleStream struct {
	text string
	step int
}

func (s *viewerLifecycleStream) Recv() (chat.MessageStreamResponse, error) {
	s.step++
	switch s.step {
	case 1:
		return chat.MessageStreamResponse{Choices: []chat.MessageStreamChoice{{Delta: chat.MessageDelta{Content: s.text}}}}, nil
	case 2:
		return chat.MessageStreamResponse{Choices: []chat.MessageStreamChoice{{FinishReason: chat.FinishReasonStop}}, Usage: &chat.Usage{InputTokens: 1, OutputTokens: 1}}, nil
	default:
		return chat.MessageStreamResponse{}, io.EOF
	}
}
func (*viewerLifecycleStream) Close() {}

type viewerLifecycleFixture struct {
	m        *model
	root     *app.App
	local    *runtime.LocalRuntime
	owner    runtime.SessionRuntimeSupervisor
	borrowed runtime.SessionRuntime
	child    runtime.SessionHandle
	node     subagent.NodeID
	services *viewerLifecycleLocalServices
	provider *viewerLifecycleProvider
}

func newViewerLifecycleFixture(t *testing.T) *viewerLifecycleFixture {
	t.Helper()
	provider := &viewerLifecycleProvider{calls: make(chan viewerLifecycleCall, 16)}
	local, err := runtime.NewLocalRuntime(t.Context(), team.New(team.WithAgents(
		agent.New("root", "fixture root", agent.WithModel(&viewerLifecycleProvider{}), agent.WithAsyncSubagents(latest.SubagentRef{Agent: "worker"})),
		agent.New("worker", "fixture worker", agent.WithModel(provider), agent.WithAsyncSubagents(latest.SubagentRef{Agent: "worker"})),
	)), runtime.WithSessionStore(session.NewInMemorySessionStore()))
	require.NoError(t, err)
	owner := runtime.NewSessionRuntimeSupervisor(local)
	t.Cleanup(func() { require.NoError(t, owner.Shutdown(context.WithoutCancel(t.Context()))) })
	borrowed := owner.Runtime()
	dir := t.TempDir()
	rootSession := session.New(session.WithID(uuid.NewString()), session.WithAgentName("root"), session.WithWorkingDir(dir))
	rootSession.AddMessage(session.UserMessage("original committed history"))
	services := &viewerLifecycleLocalServices{local: local}
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	root := app.New(ctx, borrowed, rootSession, runtime.SessionBinding{AgentName: "root"}, app.WithRuntimeServices(services))
	require.NotNil(t, root.SessionHandle())
	t.Cleanup(root.Close)
	child, err := borrowed.CreateSession(t.Context(), session.New(session.WithID(uuid.NewString())), runtime.SessionBinding{AgentName: "worker", ParentSessionID: rootSession.ID})
	require.NoError(t, err)
	node, ok := local.SubagentNodeForSession(child.ID())
	require.True(t, ok)
	require.Regexp(t, "^[0-9a-f]{5}$", string(node))
	info, ok := local.SubagentAttachInfo(node)
	require.True(t, ok)
	require.Equal(t, child.ID(), info.Session.ID)
	require.True(t, info.Session.AsyncSubagent)
	m := bareModel(80)
	m.app = root
	m.sessionState = service.NewSessionState(root.Session())
	m.viewers = &viewerHost{ctx: func() context.Context { return ctx }, events: make(chan any, 256), views: make(map[*app.App]*model)}
	t.Cleanup(m.viewers.close)
	m.subscribeViewer(ctx, root)
	root.Start(ctx)
	f := &viewerLifecycleFixture{m: m, root: root, local: local, owner: owner, borrowed: borrowed, child: child, node: node, services: services, provider: provider}
	f.event(t, root, func(msg app.SessionEventMsg) bool { _, ok := msg.Event.(*app.SessionResetEvent); return ok }, true)
	return f
}

// event consumes the actual App.Start -> canonical observation -> Subscribe ->
// viewerHost queue. A matching event can be held outside the UI to reproduce a
// switch/replacement occurring after delivery but before reduction.
func (f *viewerLifecycleFixture) event(t *testing.T, origin *app.App, matches func(app.SessionEventMsg) bool, apply bool) viewerEvent {
	t.Helper()
	timer := time.NewTimer(3 * time.Second)
	defer timer.Stop()
	for {
		select {
		case value := <-f.m.viewers.events:
			routed, ok := value.(viewerEvent)
			require.True(t, ok)
			msg, canonical := routed.event.(app.SessionEventMsg)
			if canonical && routed.origin == origin && matches(msg) {
				require.Positive(t, msg.Epoch, "must exercise a real App bridge, not epoch-zero compatibility")
				require.Equal(t, origin.Session().ID, msg.OriginSessionID)
				if apply {
					f.m.routeViewerEvent(t.Context(), routed)
				}
				return routed
			}
			f.m.routeViewerEvent(t.Context(), routed)
		case <-timer.C:
			t.Fatal("canonical viewer event did not reach the existing lean event queue")
			return viewerEvent{}
		}
	}
}

func viewerLifecycleNextCall(t *testing.T, p *viewerLifecycleProvider) viewerLifecycleCall {
	t.Helper()
	select {
	case call := <-p.calls:
		return call
	case <-time.After(3 * time.Second):
		t.Fatal("canonical session did not invoke the fixture provider")
		return viewerLifecycleCall{}
	}
}

func viewerLifecycleText(m *model) string {
	return strings.Join(m.screen.Transcript.Lines(100, 0, m.busy(), m.sessionState, nil), "\n")
}

func viewerLifecycleAttachment(t *testing.T, dir, name string) messages.Attachment {
	t.Helper()
	path := filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(path, []byte("fixture attachment "+name), 0o600))
	attachment, err := validatedAttachment(path, dir)
	require.NoError(t, err)
	return attachment
}

func (f *viewerLifecycleFixture) attach(t *testing.T, id string) *app.App {
	t.Helper()
	f.m.attachSubagentViewer(t.Context(), id)
	require.NotSame(t, f.root, f.m.app)
	require.Equal(t, f.child.ID(), f.m.app.SessionHandle().ID())
	require.Same(t, f.borrowed, f.m.app.SessionRuntime())
	require.True(t, runtime.IsLocalSessionHandle(f.m.app.SessionHandle()))
	application := f.m.app
	f.event(t, application, func(msg app.SessionEventMsg) bool { _, ok := msg.Event.(*app.SessionResetEvent); return ok }, true)
	return application
}

func TestViewerLifecycleCanonicalAttachActiveAndCompleted(t *testing.T) {
	for _, active := range []bool{true, false} {
		t.Run(map[bool]string{true: "active", false: "completed"}[active], func(t *testing.T) {
			f := newViewerLifecycleFixture(t)
			initial, err := f.child.Submit(t.Context(), runtime.TurnInput{Content: "initial child task"})
			require.NoError(t, err)
			firstCall := viewerLifecycleNextCall(t, f.provider)
			if !active {
				firstCall.reply <- "completed child history"
				require.NoError(t, f.child.AwaitTurn(t.Context(), initial.TurnID))
			}
			originalScreen := f.m.screen
			originalAttachment := viewerLifecycleAttachment(t, f.root.Session().WorkingDir, "original.txt")
			f.m.screen.Editor.SetText("original unsent draft")
			f.m.draftAttachments = []messages.Attachment{originalAttachment}
			loop := f.m.viewers
			childApp := f.attach(t, string(f.node))
			childHandle := childApp.SessionHandle()
			require.Same(t, loop, f.m.viewers, "attach stays in the existing standalone loop")
			assert.NotSame(t, originalScreen, f.m.screen)
			assert.Empty(t, f.m.screen.Editor.Text())
			assert.Empty(t, f.m.draftAttachments)
			if active {
				assert.True(t, f.m.busy(), "canonical snapshot restores an already-running child")
			} else {
				assert.False(t, f.m.busy())
				assert.Contains(t, viewerLifecycleText(f.m), "completed child history")
			}
			childScreen := f.m.screen
			childAttachment := viewerLifecycleAttachment(t, f.root.Session().WorkingDir, "child.txt")
			f.m.screen.Editor.SetText("child unsent draft")
			f.m.draftAttachments = []messages.Attachment{childAttachment}
			f.m.handleViewerCommand(t.Context(), "back", "")
			assert.Same(t, f.root, f.m.app)
			assert.Same(t, originalScreen, f.m.screen)
			assert.Equal(t, "original unsent draft", f.m.screen.Editor.Text())
			assert.Equal(t, []messages.Attachment{originalAttachment}, f.m.draftAttachments)
			assert.Contains(t, viewerLifecycleText(f.m), "original committed history")
			assert.NotContains(t, viewerLifecycleText(f.m), "completed child history")
			f.m.attachSubagentViewer(t.Context(), f.child.ID())
			assert.Same(t, childApp, f.m.app, "exact session ID returns to the existing App")
			assert.Same(t, childHandle, f.m.app.SessionHandle())
			assert.Same(t, childScreen, f.m.screen)
			assert.Equal(t, "child unsent draft", f.m.screen.Editor.Text())
			assert.Equal(t, []messages.Attachment{childAttachment}, f.m.draftAttachments)
			// Submit through the real editor path, then switch before consuming
			// the canonical acceptance and completion queued by App.Start.
			f.m.screen.Editor.SetText("explicit child follow-up")
			f.m.submitEditorMode(t.Context(), f.m.screen.Editor.Text(), busySubmitFollowUp)
			require.Empty(t, f.m.screen.Editor.Text())
			require.Empty(t, f.m.draftAttachments)
			require.Len(t, f.m.pendingUsers, 1)
			turnID := f.m.pendingUsers[0].TurnID
			require.NotEmpty(t, turnID)
			f.m.handleViewerCommand(t.Context(), "back", "")
			accepted := f.event(t, childApp, func(msg app.SessionEventMsg) bool {
				e, ok := msg.Event.(*runtime.PendingUserMessageAcceptedEvent)
				return ok && e.TurnID == turnID
			}, true)
			assert.Equal(t, turnID, accepted.event.(app.SessionEventMsg).TurnID)
			assert.Empty(t, f.m.pendingUsers, "child acceptance cannot contaminate the original composer")
			if active {
				firstCall.reply <- "initial child completed"
			}
			followCall := viewerLifecycleNextCall(t, f.provider)
			assert.Contains(t, fmt.Sprint(followCall.messages), "fixture attachment child.txt")
			assert.NotContains(t, fmt.Sprint(followCall.messages), "fixture attachment original.txt")
			followCall.reply <- "child follow-up answer"
			f.event(t, childApp, func(msg app.SessionEventMsg) bool {
				e, ok := msg.Event.(*runtime.StreamStoppedEvent)
				return ok && e.SessionID == f.child.ID() && msg.TurnID == turnID
			}, true)
			require.NoError(t, f.child.AwaitTurn(t.Context(), turnID))
			assert.NotContains(t, viewerLifecycleText(f.m), "child follow-up answer")
			assert.Equal(t, "original unsent draft", f.m.screen.Editor.Text())
			assert.Equal(t, []messages.Attachment{originalAttachment}, f.m.draftAttachments)
			f.m.attachSubagentViewer(t.Context(), f.child.ID())
			assert.Same(t, childApp, f.m.app)
			assert.Same(t, childHandle, f.m.app.SessionHandle())
			assert.Contains(t, viewerLifecycleText(f.m), "child follow-up answer")
			assert.Empty(t, f.m.pendingUsers)
			assert.False(t, f.m.busy())
			snapshot, err := f.child.Snapshot(t.Context())
			require.NoError(t, err)
			count := 0
			for _, msg := range snapshot.OwnMessages() {
				if msg.TurnID == turnID && msg.Message.Role == chat.MessageRoleUser {
					count++
					assert.NotEmpty(t, msg.Message.MultiContent, "accepted child input carries its attachment")
				}
			}
			assert.Equal(t, 1, count, "one canonical accepted user input, not a copied session or replayed send")
			assert.Zero(t, f.provider.canceled.Load(), "back/focus never cancels child execution")
		})
	}
}

func TestViewerLifecycleExactCanonicalLookupRejectsAliases(t *testing.T) {
	f := newViewerLifecycleFixture(t)
	for _, id := range []string{f.child.ID()[:8], "worker", string(f.node)[:4], "fffffff"} {
		f.m.attachSubagentViewer(t.Context(), id)
		assert.Same(t, f.root, f.m.app)
		assert.Empty(t, f.m.viewers.back)
		assert.Contains(t, viewerLifecycleText(f.m), "no longer available to open")
	}
	childApp := f.attach(t, f.child.ID())
	assert.Equal(t, f.node, childApp.AttachedSubagent().NodeID)
	f.m.handleViewerCommand(t.Context(), "back", "")
	f.m.attachSubagentViewer(t.Context(), string(f.node))
	assert.Same(t, childApp, f.m.app)
	f.m.attachSubagentViewer(t.Context(), f.child.ID())
	assert.Contains(t, viewerLifecycleText(f.m), "Already viewing this session")
	require.Len(t, f.m.viewers.back, 1)
}

func TestViewerLifecycleDeniedAdmissionPreservesDraftAndAttachments(t *testing.T) {
	for _, attached := range []bool{false, true} {
		for _, busy := range []bool{false, true} {
			t.Run(fmt.Sprintf("attachments=%v/busy=%v", attached, busy), func(t *testing.T) {
				f := newViewerLifecycleFixture(t)
				f.attach(t, string(f.node))
				draft := "retain denied child input"
				f.m.screen.Editor.SetText(draft)
				if attached {
					f.m.draftAttachments = []messages.Attachment{viewerLifecycleAttachment(t, f.root.Session().WorkingDir, "denied.txt")}
				}
				before := append([]messages.Attachment(nil), f.m.draftAttachments...)
				f.m.setTestBusy(busy)
				ctx, cancel := context.WithCancel(t.Context())
				cancel()
				f.m.submitEditorMode(ctx, draft, busySubmitFollowUp)
				assert.Equal(t, draft, f.m.screen.Editor.Text())
				assert.Equal(t, before, f.m.draftAttachments)
				assert.Empty(t, f.m.pendingUsers)
				assert.Empty(t, f.m.queue)
				assert.Contains(t, viewerLifecycleText(f.m), "context canceled")
				assert.Empty(t, f.provider.calls, "denied admission cannot reach execution")
				snapshot, err := f.child.Snapshot(t.Context())
				require.NoError(t, err)
				assert.Empty(t, snapshot.OwnMessages())
			})
		}
	}
}

func TestViewerLifecycleBufferedCanonicalEventRejectedAfterReplacement(t *testing.T) {
	for _, sameSession := range []bool{false, true} {
		t.Run(fmt.Sprintf("sameSession=%v", sameSession), func(t *testing.T) {
			f := newViewerLifecycleFixture(t)
			_, err := f.root.FollowUpMessage(t.Context(), "old root accepted input", nil)
			require.NoError(t, err)
			stale := f.event(t, f.root, func(msg app.SessionEventMsg) bool {
				_, ok := msg.Event.(*runtime.PendingUserMessageAcceptedEvent)
				return ok
			}, false)
			replacement := f.root.Session()
			if !sameSession {
				replacement = session.New(session.WithID("33333333-3333-4333-8333-333333333333"), session.WithAgentName("root"), session.WithWorkingDir(f.root.Session().WorkingDir))
			}
			f.root.ReplaceSession(t.Context(), replacement)
			// Route the first old-epoch event before receiving any new epoch.
			// A consumer's max-seen counter cannot implement this fence.
			before := len(f.m.pendingUsers)
			f.m.routeViewerEvent(t.Context(), stale)
			assert.Len(t, f.m.pendingUsers, before, "replacement must reject already-buffered positive-epoch input")
			fresh := f.event(t, f.root, func(msg app.SessionEventMsg) bool { _, ok := msg.Event.(*app.SessionResetEvent); return ok }, true)
			assert.Greater(t, fresh.event.(app.SessionEventMsg).Epoch, stale.event.(app.SessionEventMsg).Epoch)
		})
	}
}

func TestViewerLifecycleClosedHostRejectsBufferedEventWithoutStoppingBorrowedRuntime(t *testing.T) {
	f := newViewerLifecycleFixture(t)
	childApp := f.attach(t, string(f.node))
	f.m.screen.Editor.SetText("continue after detach")
	f.m.submitEditor(t.Context(), f.m.screen.Editor.Text())
	call := viewerLifecycleNextCall(t, f.provider)
	accepted := f.event(t, childApp, func(msg app.SessionEventMsg) bool {
		_, ok := msg.Event.(*runtime.PendingUserMessageAcceptedEvent)
		return ok
	}, false)
	turnID := accepted.event.(app.SessionEventMsg).TurnID
	f.m.pendingUsers = nil
	f.m.handleViewerCommand(t.Context(), "back", "")
	hidden := f.m.viewers.views[childApp]
	require.NotNil(t, hidden)
	f.m.viewers.close()
	f.m.routeViewerEvent(t.Context(), accepted)
	assert.Empty(t, hidden.pendingUsers, "closed hidden viewer must not reduce queued acceptance")
	assert.Empty(t, f.m.pendingUsers)
	assert.Empty(t, f.m.viewers.views)
	assert.Empty(t, f.m.viewers.back)
	childApp.Close()
	status, err := f.child.Status(t.Context())
	require.NoError(t, err)
	assert.Equal(t, runtime.SessionStateRunning, status.State)
	assert.Zero(t, f.provider.canceled.Load())
	call.reply <- "survived viewer shutdown"
	require.NoError(t, f.child.AwaitTurn(t.Context(), turnID))
	snapshot, err := f.child.Snapshot(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "survived viewer shutdown", snapshot.GetLastAssistantMessageContent())
	// The same canonical handle remains admissible after every observer detached.
	next, err := f.child.Submit(t.Context(), runtime.TurnInput{Content: "after observer shutdown"})
	require.NoError(t, err)
	nextCall := viewerLifecycleNextCall(t, f.provider)
	nextCall.reply <- "still resumable"
	require.NoError(t, f.child.AwaitTurn(t.Context(), next.TurnID))
}

func TestViewerLifecycleOwnedSpawnerCleanupOnlyAtFinalShutdown(t *testing.T) {
	for _, fork := range []bool{false, true} {
		t.Run(fmt.Sprintf("fork=%v", fork), func(t *testing.T) {
			f := newViewerLifecycleFixture(t)
			f.attach(t, string(f.node))
			source := f.m.app.Session()
			owned := newViewerLifecycleFixture(t)
			cleanups := 0
			f.m.spawnSession = func(_ context.Context, directory string, parent *session.Session) (*app.App, func(), error) {
				assert.Equal(t, source.WorkingDir, directory)
				if fork {
					assert.Same(t, source, parent)
				} else {
					assert.Nil(t, parent)
				}
				return owned.root, func() {
					cleanups++
					owned.root.Close()
					require.NoError(t, owned.owner.Shutdown(context.WithoutCancel(t.Context())))
				}, nil
			}
			f.m.spawnViewer(t.Context(), "", fork)
			assert.Same(t, owned.root, f.m.app)
			assert.True(t, runtime.IsLocalSessionHandle(f.m.app.SessionHandle()))
			f.m.screen.Editor.SetText("owned session draft")
			f.m.handleViewerCommand(t.Context(), "back", "")
			assert.Equal(t, f.child.ID(), f.m.app.Session().ID)
			assert.Zero(t, cleanups, "back is not ownership release")
			f.m.viewers.close()
			f.m.viewers.close()
			assert.Equal(t, 1, cleanups)
			_, err := owned.root.FollowUpMessage(t.Context(), "must be stopped", nil)
			require.Error(t, err, "owned cleanup must actually shut down its canonical runtime")
			_, err = f.child.Status(t.Context())
			require.NoError(t, err, "borrowed child is not covered by owned cleanup")
			next, err := f.child.Submit(t.Context(), runtime.TurnInput{Content: "borrowed survives owned cleanup"})
			require.NoError(t, err)
			call := viewerLifecycleNextCall(t, f.provider)
			call.reply <- "borrowed still alive"
			require.NoError(t, f.child.AwaitTurn(t.Context(), next.TurnID))
		})
	}
}

// A first-party remote SessionRuntime is exercised through an in-memory HTTP
// transport, with no listener or network dial and no substitute SessionHandle.
type viewerLifecycleRoundTripper func(*http.Request) (*http.Response, error)

func (f viewerLifecycleRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestViewerLifecycleRemoteUnsupportedAndAdmissionFailureAreTruthful(t *testing.T) {
	for _, status := range []int{http.StatusForbidden, http.StatusNotImplemented} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			var sends atomic.Int32
			client, err := runtime.NewClient("http://viewer.invalid", runtime.WithHTTPClient(&http.Client{Transport: viewerLifecycleRoundTripper(func(req *http.Request) (*http.Response, error) {
				code, body := http.StatusOK, `{"metadata":{"session_id":"remote","agent_name":"worker","capabilities":{}},"status":{"session_id":"remote","state":"settled"}}`
				if req.Method == http.MethodPost && strings.HasSuffix(req.URL.Path, "/messages") {
					sends.Add(1)
					code, body = status, `{"error":"fixture admission denied"}`
				} else if req.Method != http.MethodGet || !strings.HasSuffix(req.URL.Path, "/status") {
					return nil, fmt.Errorf("unexpected fixture request %s %s", req.Method, req.URL.Path)
				}
				return &http.Response{StatusCode: code, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
			})}))
			require.NoError(t, err)
			transport, err := runtime.NewSessionTransport(client)
			require.NoError(t, err)
			m := bareModel(80)
			m.app = app.New(t.Context(), transport, session.New(session.WithID("remote"), session.WithAgentName("worker"), session.WithWorkingDir(t.TempDir())), runtime.SessionBinding{AgentName: "worker"}, app.WithRuntimeServices(&viewerLifecycleServices{}))
			t.Cleanup(m.app.Close)
			require.NotNil(t, m.app.SessionHandle())
			assert.False(t, runtime.IsLocalSessionHandle(m.app.SessionHandle()))
			m.attachSubagentViewer(t.Context(), "abcde")
			assert.Contains(t, viewerLifecycleText(m), "no longer available to open")
			m.spawnViewer(t.Context(), "", false)
			assert.Contains(t, viewerLifecycleText(m), "not configured")
			m.screen.Editor.SetText("remote draft")
			attachment := viewerLifecycleAttachment(t, m.app.Session().WorkingDir, "remote.txt")
			m.draftAttachments = []messages.Attachment{attachment}
			m.submitEditor(t.Context(), m.screen.Editor.Text())
			assert.Equal(t, int32(1), sends.Load())
			assert.Equal(t, "remote draft", m.screen.Editor.Text())
			assert.Equal(t, []messages.Attachment{attachment}, m.draftAttachments)
			assert.Empty(t, m.pendingUsers)
			assert.False(t, m.busy())
			assert.NotContains(t, viewerLifecycleText(m), "Opened session")
			assert.NotContains(t, viewerLifecycleText(m), "Live subagent viewer")
		})
	}
}

func TestViewerLifecycleSpawnerFailureKeepsOriginalPresentation(t *testing.T) {
	f := newViewerLifecycleFixture(t)
	screen := f.m.screen
	f.m.screen.Editor.SetText("keep original draft")
	f.m.spawnSession = func(context.Context, string, *session.Session) (*app.App, func(), error) {
		return nil, nil, errors.New("fixture spawn refused")
	}
	f.m.spawnViewer(t.Context(), "", false)
	assert.Same(t, f.root, f.m.app)
	assert.Same(t, screen, f.m.screen)
	assert.Equal(t, "keep original draft", f.m.screen.Editor.Text())
	assert.Empty(t, f.m.viewers.back)
	assert.Contains(t, viewerLifecycleText(f.m), "fixture spawn refused")
	cleanups := 0
	f.m.spawnSession = func(context.Context, string, *session.Session) (*app.App, func(), error) {
		unusable := app.New(t.Context(), nil, session.New(), runtime.SessionBinding{})
		t.Cleanup(unusable.Close)
		return unusable, func() { cleanups++ }, nil
	}
	f.m.spawnViewer(t.Context(), "", true)
	assert.Same(t, f.root, f.m.app)
	assert.Equal(t, 1, cleanups, "unusable host result is cleaned immediately, not installed")
	assert.Contains(t, viewerLifecycleText(f.m), "no usable session")
	f.m.viewers.close()
	assert.Equal(t, 1, cleanups)
}

func TestViewerLifecycleAsyncCapabilityResultKeepsOriginAndRejectsStaleGeneration(t *testing.T) {
	for _, transition := range []string{"switch", "replace", "replace-same-session", "close"} {
		t.Run(transition, func(t *testing.T) {
			f := newViewerLifecycleFixture(t)
			// Hold the result until after the transition. capabilityJob captures
			// its presentation identity synchronously before starting work.
			resultGate := make(chan struct{}, 1)
			f.m.capabilityJob(t.Context(), func(ctx context.Context) (any, error) {
				select {
				case <-resultGate:
					return "origin capability marker", nil
				case <-ctx.Done():
					return nil, ctx.Err()
				}
			})
			switch transition {
			case "switch":
				f.attach(t, string(f.node))
			case "replace":
				f.root.ReplaceSession(t.Context(), session.New(session.WithID("44444444-4444-4444-8444-444444444444"), session.WithAgentName("root"), session.WithWorkingDir(f.root.Session().WorkingDir)))
			case "replace-same-session":
				f.root.ReplaceSession(t.Context(), f.root.Session())
			case "close":
				f.m.viewers.close()
			}
			resultGate <- struct{}{}
			timer := time.NewTimer(3 * time.Second)
			defer timer.Stop()
			for {
				select {
				case value := <-f.m.viewers.events:
					routed, ok := value.(viewerEvent)
					require.True(t, ok)
					f.m.routeViewerEvent(t.Context(), routed)
					if _, ok := routed.event.(capabilityResult); !ok {
						continue
					}
					assert.Same(t, f.root, routed.origin)
					assert.NotContains(t, viewerLifecycleText(f.m), "origin capability marker", "late work cannot publish into a different/closed/replaced viewer")
					if transition == "switch" {
						f.m.handleViewerCommand(t.Context(), "back", "")
						assert.Contains(t, viewerLifecycleText(f.m), "origin capability marker", "a merely hidden, still-current generation receives its result")
					}
					return
				case <-timer.C:
					t.Fatal("capability result did not reach the host queue")
					return
				}
			}
		})
	}
}

var (
	_ app.Services = (*viewerLifecycleServices)(nil)
	_ app.Services = (*viewerLifecycleLocalServices)(nil)
)

func TestViewerLifecycleStoppedAttachRequiresFreshManualInput(t *testing.T) {
	f := newViewerLifecycleFixture(t)
	store := f.local.SessionStore()
	records, err := store.(session.CoordinationStore).LoadChildren(t.Context(), f.root.Session().ID)
	require.NoError(t, err)
	require.Len(t, records, 1)
	record := records[0]
	record.Node.State = subagent.NodeStopped
	require.NoError(t, store.(session.CoordinationStore).CommitChild(t.Context(), session.ChildCommit{ExpectedRevision: record.Revision, Record: record}))
	// A cold runtime exercises the picker route without touching a terminal.
	require.NoError(t, f.owner.Shutdown(t.Context()))
	local, err := runtime.NewLocalRuntime(t.Context(), team.New(team.WithAgents(
		agent.New("root", "fixture root", agent.WithModel(&viewerLifecycleProvider{}), agent.WithAsyncSubagents(latest.SubagentRef{Agent: "worker"})),
		agent.New("worker", "fixture worker", agent.WithModel(f.provider)),
	)), runtime.WithSessionStore(store), runtime.WithWorkingDir(f.root.Session().WorkingDir))
	require.NoError(t, err)
	owner := runtime.NewSessionRuntimeSupervisor(local)
	t.Cleanup(func() { require.NoError(t, owner.Shutdown(context.WithoutCancel(t.Context()))) })
	f.local, f.owner, f.borrowed = local, owner, owner.Runtime()
	f.services.local = local
	root, err := store.GetSession(t.Context(), f.root.Session().ID)
	require.NoError(t, err)
	prepared, err := f.borrowed.(runtime.SessionViewPreparer).PrepareSessionView(t.Context(), root.ID)
	require.NoError(t, err)
	committed, err := prepared.Commit(t.Context())
	require.NoError(t, err)
	application, err := app.NewResolved(t.Context(), f.borrowed, committed, app.WithRuntimeServices(f.services))
	require.NoError(t, err)
	f.m.app, f.root = application, application
	f.m.subscribeViewer(t.Context(), application)
	childApp := f.attach(t, string(f.node))
	assert.Empty(t, f.provider.calls)
	require.NotNil(t, childApp.AttachedSubagent())
	_, err = childApp.SessionHandle().Retry(t.Context())
	require.Error(t, err)
	submission, err := childApp.SessionHandle().Submit(t.Context(), runtime.TurnInput{Content: "manual revival"})
	require.NoError(t, err)
	call := viewerLifecycleNextCall(t, f.provider)
	call.reply <- "revived response"
	require.NoError(t, childApp.SessionHandle().AwaitTurn(t.Context(), submission.TurnID))
	assert.Empty(t, f.provider.calls)
}

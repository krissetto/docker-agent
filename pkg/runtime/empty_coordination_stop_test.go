package runtime

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/subagent"
)

type emptyCoordinationObserver struct {
	mu     sync.Mutex
	events map[string][]Event
}

func (*emptyCoordinationObserver) OnRunStart(context.Context, *session.Session) {}
func (o *emptyCoordinationObserver) OnEvent(_ context.Context, sess *session.Session, event Event) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.events[sess.ID] = append(o.events[sess.ID], event)
}

func (o *emptyCoordinationObserver) diagnostics(id string) (warnings, failures []string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	for _, event := range o.events[id] {
		switch e := event.(type) {
		case *WarningEvent:
			if strings.Contains(e.Message, "empty response") || strings.Contains(e.Message, "only reasoning") || strings.Contains(e.Message, "refused") {
				warnings = append(warnings, e.Message)
			}
		case *ErrorEvent:
			failures = append(failures, e.Error)
		}
	}
	return warnings, failures
}

func emptyCoordinationStream(reason chat.FinishReason, reasoning string) *mockStream {
	stream := newStreamBuilder().AddReasoning(reasoning).Build()
	stream.responses = append(stream.responses, chat.MessageStreamResponse{Choices: []chat.MessageStreamChoice{{FinishReason: reason}}})
	return stream
}

// Exercise the actual send_message handler, child settlement/outbox, parent
// admission and both execution loops. The sender's post-tool stop was already
// benign; the receiving notification-only wake used to warn instead.
func TestEmptyCoordinationSendMessageAndReport(t *testing.T) {
	for _, reasoning := range []string{"", "I have sent the update and have nothing to add."} {
		t.Run(reasoning, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				observer := &emptyCoordinationObserver{events: map[string][]Event{}}
				var workerCalls, parentCalls atomic.Int32
				rootProvider := coordinationReply("")
				rootProvider.call = func(context.Context, []chat.Message) (chat.MessageStream, error) {
					parentCalls.Add(1)
					return emptyCoordinationStream(chat.FinishReasonStop, reasoning), nil
				}
				worker := coordinationReply("")
				worker.call = func(context.Context, []chat.Message) (chat.MessageStream, error) {
					if workerCalls.Add(1) == 1 {
						return newStreamBuilder().AddToolCallName("send", subagent.ToolSendMessage).AddToolCallArguments("send", `{"to":"parent","message":"The requested update."}`).AddToolCallStopWithUsage(0, 0).Build(), nil
					}
					return emptyCoordinationStream(chat.FinishReasonStop, reasoning), nil
				}
				store := session.NewInMemorySessionStore()
				_, owner := coordinationRuntime(t, store, rootProvider, worker, WithEventObserver(observer))
				parent := coordinationCreate(t, owner.Runtime(), "parent", "")
				child := coordinationCreate(t, owner.Runtime(), "child", parent.ID())
				_, err := child.Edit(t.Context(), SessionEdit{Kind: SessionEditPolicy, ToolsApproved: new(true)})
				require.NoError(t, err)
				accepted, err := child.Submit(t.Context(), TurnInput{Content: "Send an update", RequestID: "work"})
				require.NoError(t, err)
				coordinationAwait(t, child, accepted.TurnID)
				synctest.Wait()
				assert.Equal(t, int32(2), workerCalls.Load())
				assert.Positive(t, parentCalls.Load())
				assert.LessOrEqual(t, parentCalls.Load(), int32(2), "one explicit message and one automatic report, no empty extra wake")
				for _, id := range []string{parent.ID(), child.ID()} {
					warnings, failures := observer.diagnostics(id)
					assert.Empty(t, warnings, id)
					assert.Empty(t, failures, id)
				}
				snapshot, err := parent.Snapshot(t.Context())
				require.NoError(t, err)
				var explicit, reports, assistant int
				for _, item := range snapshot.MessagesSnapshot() {
					if msg := item.Message; msg != nil {
						assert.False(t, msg.Pending)
						if msg.InputOrigin == session.InputOriginAgent {
							explicit++
						}
						if strings.HasPrefix(msg.TurnID, "report:") {
							reports++
							assert.Equal(t, "report:"+childReportID(child.ID(), accepted.TurnID), msg.TurnID)
						}
						if msg.Message.Role == chat.MessageRoleAssistant {
							assistant++
						}
					}
				}
				assert.Equal(t, 1, explicit)
				assert.Equal(t, 1, reports)
				assert.Zero(t, assistant, "do not fabricate an answer or promote reasoning")
				pending, err := store.(session.CoordinationStore).PendingReports(t.Context(), parent.ID())
				require.NoError(t, err)
				assert.Empty(t, pending)
				records, err := store.(session.CoordinationStore).LoadChildren(t.Context(), parent.ID())
				require.NoError(t, err)
				require.Len(t, records, 1)
				assert.Equal(t, accepted.TurnID, records[0].LastTurnID)
				assert.Empty(t, records[0].Result)
				assert.Equal(t, subagent.NodeIdle, records[0].Node.State)
			})
		})
	}
}

func TestEmptyCoordinationReportFinishClassification(t *testing.T) {
	for _, tc := range []struct {
		name      string
		reason    chat.FinishReason
		reasoning string
		streamErr bool
		warning   bool
	}{
		{"empty stop", chat.FinishReasonStop, "", false, false},
		{"reasoning stop", chat.FinishReasonStop, "No response needed.", false, false},
		{"length", chat.FinishReasonLength, "", false, true},
		{"reasoning length", chat.FinishReasonLength, "Still working", false, true},
		{"unknown EOF", "", "", false, true},
		{"refusal", chat.FinishReasonRefusal, "", false, true},
		{"provider error", "", "", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				observer := &emptyCoordinationObserver{events: map[string][]Event{}}
				var calls atomic.Int32
				p := coordinationReply("")
				p.call = func(context.Context, []chat.Message) (chat.MessageStream, error) {
					calls.Add(1)
					if tc.streamErr {
						return nil, errors.New("synthetic provider failure")
					}
					return emptyCoordinationStream(tc.reason, tc.reasoning), nil
				}
				_, owner := coordinationRuntime(t, session.NewInMemorySessionStore(), p, coordinationReply("done"), WithEventObserver(observer))
				parent := coordinationCreate(t, owner.Runtime(), "parent", "")
				child := coordinationCreate(t, owner.Runtime(), "child", parent.ID())
				accepted, err := child.Submit(t.Context(), TurnInput{Content: "work", RequestID: "work"})
				require.NoError(t, err)
				coordinationAwait(t, child, accepted.TurnID)
				synctest.Wait()
				assert.Equal(t, int32(1), calls.Load(), "one report creates exactly one wake")
				warnings, failures := observer.diagnostics(parent.ID())
				assert.Equal(t, tc.warning, len(warnings) > 0)
				assert.Equal(t, tc.streamErr, len(failures) > 0)
			})
		})
	}
}

func TestEmptyCoordinationInputBoundaries(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		rt, owner := coordinationRuntime(t, session.NewInMemorySessionStore(), coordinationReply("done"), coordinationReply("done"))
		parent := coordinationCreate(t, owner.Runtime(), "parent", "")
		child := coordinationCreate(t, owner.Runtime(), "child", parent.ID())
		parentSession := parent.(*sessionHandle).driver.session()
		childSession := child.(*sessionHandle).driver.session()
		report := QueuedMessage{InputOrigin: session.InputOriginRuntime, SenderID: child.ID(), RequestID: "report:" + childReportID(child.ID(), "work"), InputMode: "steer"}
		for _, tc := range []struct {
			name  string
			sess  *session.Session
			msg   QueuedMessage
			quiet bool
		}{
			{"report", parentSession, report, true},
			{"explicit child message", parentSession, QueuedMessage{InputOrigin: session.InputOriginAgent, SenderID: child.ID(), InputMode: "steer"}, true},
			{"parent task", childSession, QueuedMessage{InputOrigin: session.InputOriginAgent, SenderID: parent.ID(), InputMode: "steer"}, false},
			{"user", parentSession, QueuedMessage{InputOrigin: session.InputOriginUser}, false},
			{"runtime note", parentSession, QueuedMessage{InputOrigin: session.InputOriginRuntime, SenderID: child.ID(), InputMode: "steer", RequestID: "note"}, false},
			{"unrelated sender", parentSession, QueuedMessage{InputOrigin: session.InputOriginAgent, SenderID: "unrelated", InputMode: "steer"}, false},
			{"retry", parentSession, QueuedMessage{Retry: true}, false},
		} {
			assert.Equal(t, tc.quiet, rt.quietCoordinationInput(tc.sess, tc.msg), tc.name)
		}
		for _, inputs := range [][]bool{{false, true}, {true, false}, {true, false, true}} {
			ls := &loopState{prevTurnMadeToolCalls: true}
			for _, quiet := range inputs {
				ls.consumeTurnInput(quiet)
			}
			assert.False(t, ls.quietCoordinationTurn, "mixed input is not report-only")
			assert.False(t, ls.prevTurnMadeToolCalls, "new unanswered user input resets old tool exemption")
			assert.NotEmpty(t, emptyTurnWarning(streamResult{}, ls.prevTurnMadeToolCalls || ls.quietCoordinationTurn, "model", chat.FinishReasonStop))
		}
	})
}

func TestEmptyCoordinationDoesNotHideUnansweredInput(t *testing.T) {
	for _, mode := range []string{"user", "parent task", "runtime note", "mixed user and report"} {
		for _, reasoning := range []string{"", "Thinking without an answer"} {
			t.Run(mode+"/"+reasoning, func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					observer := &emptyCoordinationObserver{events: map[string][]Event{}}
					var calls atomic.Int32
					entered, release := make(chan struct{}), make(chan struct{})
					var releaseOnce sync.Once
					defer releaseOnce.Do(func() { close(release) })
					p := coordinationReply("")
					p.call = func(ctx context.Context, _ []chat.Message) (chat.MessageStream, error) {
						if calls.Add(1) == 1 && mode == "mixed user and report" {
							close(entered)
							return &steeringBoundaryStream{done: ctx.Done(), err: ctx.Err, release: release, chunks: emptyCoordinationStream(chat.FinishReasonStop, reasoning).responses}, nil
						}
						return emptyCoordinationStream(chat.FinishReasonStop, reasoning), nil
					}
					rootProvider, worker := p, coordinationReply("done")
					if mode == "parent task" {
						rootProvider, worker = coordinationReply("received"), p
					}
					rt, owner := coordinationRuntime(t, session.NewInMemorySessionStore(), rootProvider, worker, WithEventObserver(observer))
					parent := coordinationCreate(t, owner.Runtime(), "parent", "")
					child := coordinationCreate(t, owner.Runtime(), "child", parent.ID())
					target := parent
					switch mode {
					case "parent task":
						target = child
						require.True(t, rt.subagents.deliverAgent(child.ID(), "Please do work", parent.ID(), "root"))
					case "runtime note":
						require.True(t, parent.(*sessionHandle).driver.postTrustedInput(t.Context(), QueuedMessage{Content: "Unrelated runtime note", InputOrigin: session.InputOriginRuntime, SenderID: child.ID(), InputMode: "steer"}))
					default:
						_, err := parent.Submit(t.Context(), TurnInput{Content: "Answer my question", RequestID: "question"})
						require.NoError(t, err)
						if mode == "mixed user and report" {
							coordinationWait(t, entered)
							turn, err := child.Submit(t.Context(), TurnInput{Content: "Finish work", RequestID: "work"})
							require.NoError(t, err)
							coordinationAwait(t, child, turn.TurnID)
							releaseOnce.Do(func() { close(release) })
						}
					}
					synctest.Wait()
					warnings, failures := observer.diagnostics(target.ID())
					require.Len(t, warnings, 1, "unanswered work must not become a benign notification")
					assert.Empty(t, failures)
					assert.NotContains(t, warnings[0], "rate limit")
					if reasoning != "" {
						assert.Contains(t, warnings[0], "only reasoning")
					} else {
						assert.Contains(t, warnings[0], "empty response")
					}
					expectedCalls := int32(1)
					if mode == "mixed user and report" {
						expectedCalls = 2
					}
					assert.Equal(t, expectedCalls, calls.Load())
				})
			})
		}
	}
}

// A report can interrupt the post-tool stream at the earliest safe boundary.
// That interruption is not a completed tool-free turn and must not erase the
// sender's already completed tool work.
func TestEmptyCoordinationInterruptedPostToolStop(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		observer := &emptyCoordinationObserver{events: map[string][]Event{}}
		entered, release := make(chan struct{}), make(chan struct{})
		var releaseOnce sync.Once
		defer releaseOnce.Do(func() { close(release) })
		var calls atomic.Int32
		var childNode subagent.NodeID
		p := coordinationReply("")
		p.call = func(ctx context.Context, _ []chat.Message) (chat.MessageStream, error) {
			switch calls.Add(1) {
			case 1:
				return newStreamBuilder().AddToolCallName("send", subagent.ToolSendMessage).AddToolCallArguments("send", fmt.Sprintf(`{"to":%q,"message":"Do the work"}`, childNode)).AddToolCallStopWithUsage(0, 0).Build(), nil
			case 2:
				close(entered)
				return &steeringBoundaryStream{done: ctx.Done(), err: ctx.Err, release: release, chunks: emptyCoordinationStream(chat.FinishReasonStop, "").responses}, nil
			default:
				return emptyCoordinationStream(chat.FinishReasonStop, "Nothing more to add."), nil
			}
		}
		worker := coordinationReply("done")
		worker.call = func(ctx context.Context, _ []chat.Message) (chat.MessageStream, error) {
			select {
			case <-entered:
				releaseOnce.Do(func() { close(release) })
				return newStreamBuilder().AddContent("done").AddStopWithUsage(0, 0).Build(), nil
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		rt, owner := coordinationRuntime(t, session.NewInMemorySessionStore(), p, worker, WithEventObserver(observer))
		rootAgent, err := rt.team.Agent("root")
		require.NoError(t, err)
		agent.WithToolSets(subagent.NewToolSet())(rootAgent)
		parent := coordinationCreate(t, owner.Runtime(), "parent", "")
		_, err = parent.Edit(t.Context(), SessionEdit{Kind: SessionEditPolicy, ToolsApproved: new(true)})
		require.NoError(t, err)
		child := coordinationCreate(t, owner.Runtime(), "child", parent.ID())
		var ok bool
		childNode, ok = rt.subagents.nodeForSession(child.ID())
		require.True(t, ok)
		accepted, err := parent.Submit(t.Context(), TurnInput{Content: "Delegate the work", RequestID: "delegate"})
		require.NoError(t, err)
		coordinationWait(t, entered)
		releaseOnce.Do(func() { close(release) })
		coordinationAwait(t, parent, accepted.TurnID)
		synctest.Wait()
		assert.GreaterOrEqual(t, calls.Load(), int32(2), "report is consumed after the trailing provider completes")
		warnings, failures := observer.diagnostics(parent.ID())
		assert.Empty(t, warnings)
		assert.Empty(t, failures)
	})
}

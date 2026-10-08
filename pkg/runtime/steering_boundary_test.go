package runtime

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/modelerrors"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/subagent"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/tools"
)

// The provider never completes until explicitly released or its attempt is canceled.
type steeringBoundaryStream struct {
	done    <-chan struct{}
	err     func() error
	chunks  []chat.MessageStreamResponse
	release <-chan struct{}
}

func (s *steeringBoundaryStream) Recv() (chat.MessageStreamResponse, error) {
	if len(s.chunks) != 0 {
		chunk := s.chunks[0]
		s.chunks = s.chunks[1:]
		return chunk, nil
	}
	select {
	case <-s.release:
		return chat.MessageStreamResponse{}, io.EOF
	case <-s.done:
		return chat.MessageStreamResponse{}, s.err()
	}
}

func (*steeringBoundaryStream) Close() {}

func TestSteeringBoundaryInterruptsProvider(t *testing.T) {
	for _, name := range []string{"content", "partial_tool", "partial_only", "provider_creation", "retry_backoff", "retry_after"} {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				firstRelease, restartRelease := make(chan struct{}), make(chan struct{})
				var firstOnce, restartOnce sync.Once
				defer firstOnce.Do(func() { close(firstRelease) })
				defer restartOnce.Do(func() { close(restartRelease) })
				var calls atomic.Int32
				var restarted []chat.Message
				restartEntered := make(chan struct{})
				provider := coordinationReply("done")
				provider.call = func(ctx context.Context, messages []chat.Message) (chat.MessageStream, error) {
					switch calls.Add(1) {
					case 1:
						if name == "retry_backoff" {
							return nil, errors.New("503 service unavailable")
						}
						if name == "retry_after" {
							return nil, &modelerrors.StatusError{StatusCode: 429, RetryAfter: time.Hour, Err: errors.New("rate limited")}
						}
						if name == "provider_creation" {
							select {
							case <-firstRelease:
							case <-ctx.Done():
								return nil, ctx.Err()
							}
						}
						chunks := newStreamBuilder()
						if name != "partial_only" {
							chunks.AddContent("consumed content").AddReasoning("consumed reasoning")
						}
						if name == "partial_tool" || name == "partial_only" {
							chunks.AddToolCallName("uncommitted", "must_not_execute").AddToolCallArguments("uncommitted", `{"unfinished":`)
						}
						return &steeringBoundaryStream{done: ctx.Done(), err: ctx.Err, chunks: chunks.responses, release: firstRelease}, nil
					case 2:
						restarted = messages
						close(restartEntered)
						select {
						case <-restartRelease:
						case <-ctx.Done():
							return nil, ctx.Err()
						}
					}
					return newStreamBuilder().AddContent("done").AddStopWithUsage(1, 1).Build(), nil
				}
				store := session.NewInMemorySessionStore()
				_, owner := coordinationRuntime(t, store, provider, coordinationReply("unused"), WithRetryOnRateLimit())
				h := coordinationCreate(t, owner.Runtime(), "boundary", "")
				// These releases run before supervisor shutdown, including failing assertions.
				t.Cleanup(func() { firstOnce.Do(func() { close(firstRelease) }); restartOnce.Do(func() { close(restartRelease) }) })
				active, err := h.Submit(t.Context(), TurnInput{Content: "active", RequestID: "active"})
				require.NoError(t, err)
				synctest.Wait()
				queued, err := h.Submit(t.Context(), TurnInput{Content: "ordinary queued", RequestID: "ordinary"})
				require.NoError(t, err)
				d := h.(*sessionHandle).driver
				guide, err := h.Steer(t.Context(), TurnInput{Content: "user guidance", RequestID: "guidance"})
				require.NoError(t, err)
				synctest.Wait()
				select {
				case <-restartEntered:
				default:
					t.Fatal("steering must restart without releasing the unfinished provider stream")
				}
				assert.Equal(t, active.TurnID, d.ActiveRequestID())
				var contents []string
				for _, message := range restarted {
					contents = append(contents, message.Content)
					assert.Empty(t, message.ToolCalls, "uncommitted calls must not enter provider history")
					if message.Content == "consumed content" {
						assert.Equal(t, "consumed reasoning", message.ReasoningContent)
					}
				}
				if name == "content" || name == "partial_tool" {
					assert.Contains(t, contents, "consumed content")
				}
				assert.Contains(t, contents, "user guidance")
				assert.NotContains(t, contents, "ordinary queued")
				coordinationAwait(t, h, guide.TurnID)
				restartOnce.Do(func() { close(restartRelease) })
				coordinationAwait(t, h, queued.TurnID)
				coordinationSettled(t, h)
				assert.Equal(t, int32(3), calls.Load())
				persisted, err := store.GetSession(t.Context(), h.ID())
				require.NoError(t, err)
				live, err := h.Snapshot(t.Context())
				require.NoError(t, err)
				assert.Len(t, persisted.MessagesSnapshot(), len(live.MessagesSnapshot()), "boundary-only events must not create phantom persisted messages")
				for _, item := range persisted.MessagesSnapshot() {
					if item.Message != nil && item.Message.Message.Role == chat.MessageRoleAssistant {
						assert.NotEmpty(t, item.Message.Message.Content)
						assert.Empty(t, item.Message.Message.ToolCalls)
					}
				}
			})
		})
	}
}

func TestSteeringBoundaryConsumedAwaitNotifies(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := newDriverTestRuntime(t)
		d := r.sessionDrivers.Get(session.New(session.WithID("await-steering")))
		h := &sessionHandle{driver: d, sessionID: "await-steering"}
		input, err := h.Steer(t.Context(), TurnInput{Content: "guidance"})
		require.NoError(t, err)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		done := make(chan error, 1)
		go func() { done <- h.AwaitTurn(ctx, input.TurnID) }()
		synctest.Wait()
		require.Len(t, r.drainSessionSteer(h.ID()), 1)
		synctest.Wait()
		select {
		case err := <-done:
			require.NoError(t, err)
		default:
			t.Fatal("consumed steering must notify an already waiting AwaitTurn")
		}
	})
}

func TestSteeringBoundaryAfterStreamCompletionCannotStrandInput(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		stopped, release := make(chan struct{}), make(chan struct{})
		var once sync.Once
		var stops atomic.Int32
		observer := &fnObserver{onEvent: func(_ context.Context, _ *session.Session, event Event) {
			if _, ok := event.(*StreamStoppedEvent); ok && stops.Add(1) == 1 {
				close(stopped)
				<-release
			}
		}}
		_, owner := coordinationRuntime(t, session.NewInMemorySessionStore(), coordinationReply("done"), coordinationReply("unused"), WithEventObserver(observer))
		t.Cleanup(func() { once.Do(func() { close(release) }) })
		h := coordinationCreate(t, owner.Runtime(), "completed-boundary", "")
		active, err := h.Submit(t.Context(), TurnInput{Content: "active"})
		require.NoError(t, err)
		<-stopped
		guide, err := h.Steer(t.Context(), TurnInput{Content: "late guidance"})
		require.NoError(t, err)
		once.Do(func() { close(release) })
		synctest.Wait()
		d := h.(*sessionHandle).driver
		d.mu.Lock()
		assert.Empty(t, d.steering, "steering accepted after final loop drain must not remain stranded")
		d.mu.Unlock()
		coordinationAwait(t, h, active.TurnID)
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		assert.NoError(t, h.AwaitTurn(ctx, guide.TurnID), "completed successor must settle the late input")
	})
}

func TestSteeringBoundaryActiveToolsDrainAllOriginsTogether(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		toolEntered, toolRelease, restartRelease := make(chan struct{}), make(chan struct{}), make(chan struct{})
		var toolOnce, restartOnce sync.Once
		var calls atomic.Int32
		var restarted []chat.Message
		listingRelease := make(chan struct{})
		var listingOnce sync.Once
		listing := &steeringListingGate{release: listingRelease}
		provider := coordinationReply("done")
		provider.call = func(ctx context.Context, messages []chat.Message) (chat.MessageStream, error) {
			switch calls.Add(1) {
			case 1:
				return newStreamBuilder().AddToolCallName("committed", "blocking").AddToolCallArguments("committed", `{}`).AddToolCallStopWithUsage(1, 1).Build(), nil
			case 2:
				restarted = messages
				select {
				case <-restartRelease:
				case <-ctx.Done():
					return nil, ctx.Err()
				}
			}
			return newStreamBuilder().AddContent("done").AddStopWithUsage(1, 1).Build(), nil
		}
		tool := tools.Tool{Name: "blocking", Parameters: map[string]any{}, Handler: func(ctx context.Context, _ tools.ToolCall, _ tools.Runtime) (*tools.ToolCallResult, error) {
			close(toolEntered)
			select {
			case <-toolRelease:
				listing.armed.Store(true)
				return tools.ResultSuccess("paired result"), nil
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}}
		listing.tool = tool
		r, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(agent.New("root", "prompt", agent.WithModel(provider), agent.WithToolSets(listing)))), WithSessionCompaction(false), WithModelStore(mockModelStore{}))
		require.NoError(t, err)
		owner := NewSessionRuntimeSupervisor(r)
		t.Cleanup(func() { require.NoError(t, owner.Shutdown(context.WithoutCancel(t.Context()))) })
		t.Cleanup(func() {
			toolOnce.Do(func() { close(toolRelease) })
			restartOnce.Do(func() { close(restartRelease) })
			listingOnce.Do(func() { close(listingRelease) })
		})
		h, err := owner.Runtime().CreateSession(t.Context(), session.New(session.WithID("tools-boundary"), session.WithTitle("test"), session.WithToolsApproved(true)), SessionBinding{AgentName: "root"})
		require.NoError(t, err)
		active, err := h.Submit(t.Context(), TurnInput{Content: "active"})
		require.NoError(t, err)
		<-toolEntered
		ordinary, err := h.Submit(t.Context(), TurnInput{Content: "ordinary queued"})
		require.NoError(t, err)
		d := h.(*sessionHandle).driver
		guide, err := h.Steer(t.Context(), TurnInput{Content: "user guidance"})
		require.NoError(t, err)
		for _, origin := range []session.InputOrigin{session.InputOriginAgent, session.InputOriginRuntime} {
			input := QueuedMessage{Content: string(origin) + " guidance", RequestID: string(origin), InputOrigin: origin, InputMode: "steer"}
			require.True(t, d.Post(t.Context(), input, true))
			require.True(t, d.Post(t.Context(), input, true), "same request is idempotent")
		}
		synctest.Wait()
		assert.Equal(t, int32(1), calls.Load(), "steering must not interrupt active tool execution")
		assert.Equal(t, active.TurnID, d.ActiveRequestID())
		toolOnce.Do(func() { close(toolRelease) })
		synctest.Wait()
		snapshot, err := h.Snapshot(t.Context())
		require.NoError(t, err)
		for _, item := range snapshot.MessagesSnapshot() {
			if item.Message != nil && item.Message.TurnID == guide.TurnID {
				assert.False(t, item.Message.Pending, "paired-result steering must precede optional toolset reprobe")
			}
		}
		listingOnce.Do(func() { close(listingRelease) })
		synctest.Wait()
		require.Equal(t, int32(2), calls.Load())
		var contents []string
		callPosition, resultPosition := -1, -1
		for i, message := range restarted {
			contents = append(contents, message.Content)
			if len(message.ToolCalls) != 0 {
				callPosition = i
			}
			if message.Role == chat.MessageRoleTool {
				resultPosition = i
				assert.Equal(t, "committed", message.ToolCallID)
			}
		}
		assert.Greater(t, resultPosition, callPosition)
		canonical, err := h.Snapshot(t.Context())
		require.NoError(t, err)
		for _, item := range canonical.MessagesSnapshot() {
			if item.Message != nil && item.Message.TurnID == guide.TurnID {
				assert.Equal(t, "user guidance", item.Message.Message.Content, "separator is private model assembly, not canonical input")
			}
		}
		assert.Contains(t, contents, "user guidance\n")
		assert.NotContains(t, contents, "ordinary queued")
		for _, body := range []string{"runtime guidance", "agent guidance"} {
			count := 0
			for _, content := range contents {
				if strings.Contains(content, body) {
					count++
				}
			}
			assert.Equal(t, 1, count)
		}
		coordinationAwait(t, h, guide.TurnID)
		restartOnce.Do(func() { close(restartRelease) })
		coordinationAwait(t, h, ordinary.TurnID)
		coordinationSettled(t, h)
		assert.Equal(t, int32(3), calls.Load())
	})
}

func TestSteeringBoundaryPromotionWaitsForObservedAssistant(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		providerRelease, boundaryRelease := make(chan struct{}), make(chan struct{})
		var providerOnce, boundaryOnce sync.Once
		var calls atomic.Int32
		var gated atomic.Bool
		observer := &fnObserver{onEvent: func(_ context.Context, _ *session.Session, event Event) {
			if added, ok := event.(*MessageAddedEvent); ok && added.Message != nil && added.Message.Message.Role == chat.MessageRoleAssistant && gated.CompareAndSwap(false, true) {
				<-boundaryRelease
			}
		}}
		provider := coordinationReply("done")
		provider.call = func(ctx context.Context, _ []chat.Message) (chat.MessageStream, error) {
			if calls.Add(1) == 1 {
				return &steeringBoundaryStream{done: ctx.Done(), err: ctx.Err, chunks: newStreamBuilder().AddContent("old answer").responses, release: providerRelease}, nil
			}
			return newStreamBuilder().AddContent("new answer").AddStopWithUsage(1, 1).Build(), nil
		}
		_, owner := coordinationRuntime(t, session.NewInMemorySessionStore(), provider, coordinationReply("unused"), WithEventObserver(observer))
		t.Cleanup(func() {
			providerOnce.Do(func() { close(providerRelease) })
			boundaryOnce.Do(func() { close(boundaryRelease) })
		})
		h := coordinationCreate(t, owner.Runtime(), "observed-boundary", "")
		_, err := h.Submit(t.Context(), TurnInput{Content: "active"})
		require.NoError(t, err)
		synctest.Wait()
		guide, err := h.Steer(t.Context(), TurnInput{Content: "guidance"})
		require.NoError(t, err)
		providerOnce.Do(func() { close(providerRelease) })
		synctest.Wait()
		require.True(t, gated.Load())
		snapshot, err := h.Snapshot(t.Context())
		require.NoError(t, err)
		for _, item := range snapshot.MessagesSnapshot() {
			if item.Message != nil && item.Message.TurnID == guide.TurnID {
				assert.True(t, item.Message.Pending, "promotion must await the canonical assistant boundary, not merely its buffered send")
			}
		}
		assert.Equal(t, int32(1), calls.Load(), "restart cannot overtake blocked assistant observation")
		boundaryOnce.Do(func() { close(boundaryRelease) })
		coordinationAwait(t, h, guide.TurnID)
		coordinationSettled(t, h)
	})
}

func TestSteeringBoundaryCancelOwnedTurnPreservesQueuedTurn(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		firstRelease, restartRelease, queuedRelease := make(chan struct{}), make(chan struct{}), make(chan struct{})
		var releases sync.Once
		var calls atomic.Int32
		provider := coordinationReply("done")
		provider.call = func(ctx context.Context, _ []chat.Message) (chat.MessageStream, error) {
			switch calls.Add(1) {
			case 1:
				return &steeringBoundaryStream{done: ctx.Done(), err: ctx.Err, chunks: newStreamBuilder().AddContent("partial").responses, release: firstRelease}, nil
			case 2:
				select {
				case <-restartRelease:
				case <-ctx.Done():
					return nil, ctx.Err()
				}
			case 3:
				select {
				case <-queuedRelease:
				case <-ctx.Done():
					return nil, ctx.Err()
				}
			}
			return newStreamBuilder().AddContent("done").AddStopWithUsage(1, 1).Build(), nil
		}
		_, owner := coordinationRuntime(t, session.NewInMemorySessionStore(), provider, coordinationReply("unused"))
		t.Cleanup(func() { releases.Do(func() { close(firstRelease); close(restartRelease); close(queuedRelease) }) })
		h := coordinationCreate(t, owner.Runtime(), "cancel-boundary", "")
		active, err := h.Submit(t.Context(), TurnInput{Content: "active"})
		require.NoError(t, err)
		synctest.Wait()
		ordinary, err := h.Submit(t.Context(), TurnInput{Content: "ordinary"})
		require.NoError(t, err)
		_, err = h.Steer(t.Context(), TurnInput{Content: "guidance"})
		require.NoError(t, err)
		synctest.Wait()
		require.Equal(t, int32(2), calls.Load())
		result, err := h.Cancel(t.Context(), active.TurnID)
		require.NoError(t, err)
		assert.Equal(t, CancelAccepted, result.Outcome)
		synctest.Wait()
		assert.Equal(t, int32(3), calls.Load())
		assert.Equal(t, ordinary.TurnID, h.(*sessionHandle).driver.ActiveRequestID())
		result, err = h.Cancel(t.Context(), active.TurnID)
		require.NoError(t, err)
		assert.Equal(t, CancelNotActive, result.Outcome)
		releases.Do(func() { close(firstRelease); close(restartRelease); close(queuedRelease) })
		coordinationAwait(t, h, ordinary.TurnID)
		coordinationSettled(t, h)
	})
}

// Armed by tool completion, this catches any post-result tool discovery before promotion.
type steeringListingGate struct {
	tool    tools.Tool
	armed   atomic.Bool
	release <-chan struct{}
}

func (g *steeringListingGate) Tools(ctx context.Context) ([]tools.Tool, error) {
	if g.armed.Load() {
		select {
		case <-g.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return []tools.Tool{g.tool}, nil
}

func TestSteeringBoundaryReservedCompaction(t *testing.T) {
	for _, kind := range []string{"user", "agent", "runtime_report", "new_turn"} {
		t.Run(kind, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				firstRelease, summaryRelease, restartRelease := make(chan struct{}), make(chan struct{}), make(chan struct{})
				var firstOnce, summaryOnce, restartOnce sync.Once
				var calls, summaryCalls atomic.Int32
				var attempt context.Context
				var restarted, summaryInput []chat.Message
				summaryEntered, restartEntered := make(chan struct{}), make(chan struct{})
				provider := coordinationReply("done")
				provider.call = func(ctx context.Context, messages []chat.Message) (chat.MessageStream, error) {
					if calls.Add(1) == 1 {
						attempt = ctx
						return &steeringBoundaryStream{done: ctx.Done(), err: ctx.Err, chunks: newStreamBuilder().AddContent("old partial").responses, release: firstRelease}, nil
					}
					restarted = messages
					close(restartEntered)
					select {
					case <-restartRelease:
					case <-ctx.Done():
						return nil, ctx.Err()
					}
					return newStreamBuilder().AddContent("done").AddStopWithUsage(1, 1).Build(), nil
				}
				summarizer := coordinationReply("reserved summary")
				summarizer.call = func(ctx context.Context, messages []chat.Message) (chat.MessageStream, error) {
					summaryCalls.Add(1)
					summaryInput = messages
					close(summaryEntered)
					select {
					case <-summaryRelease:
					case <-ctx.Done():
						return nil, ctx.Err()
					}
					return newStreamBuilder().AddContent("reserved summary").AddStopWithUsage(10, 5).Build(), nil
				}
				store := session.NewInMemorySessionStore()
				a := agent.New("root", "prompt", agent.WithModel(provider), agent.WithCompactionModel(summarizer))
				r, err := NewLocalRuntime(t.Context(), team.New(team.WithAgents(a)), WithSessionStore(store), WithSessionCompaction(false), WithModelStore(mockModelStoreWithLimit{limit: 100_000}))
				require.NoError(t, err)
				owner := NewSessionRuntimeSupervisor(r)
				t.Cleanup(func() { require.NoError(t, owner.Shutdown(context.WithoutCancel(t.Context()))) })
				t.Cleanup(func() {
					firstOnce.Do(func() { close(firstRelease) })
					summaryOnce.Do(func() { close(summaryRelease) })
					restartOnce.Do(func() { close(restartRelease) })
				})
				h := coordinationCreate(t, owner.Runtime(), "reserved-boundary", "")
				active, err := h.Submit(t.Context(), TurnInput{Content: "active", RequestID: "active"})
				require.NoError(t, err)
				synctest.Wait()
				compactEvents := make(chan Event, 16)
				require.NoError(t, h.Compact(t.Context(), "", NewChannelSink(compactEvents)))
				d := h.(*sessionHandle).driver
				requestID := "communication"
				switch kind {
				case "user":
					input, err := h.Steer(t.Context(), TurnInput{Content: "STEERING correction", RequestID: requestID})
					require.NoError(t, err)
					requestID = input.TurnID
				default:
					mode := subagent.DeliveryGuidance
					if kind == "new_turn" {
						mode = subagent.DeliveryNewTurn
					}
					input := agentCommunication("STEERING correction", requestID, "sender", "worker", mode)
					if kind == "runtime_report" {
						input.InputOrigin = session.InputOriginRuntime
					}
					receipt, err := d.postCommunication(t.Context(), input)
					require.NoError(t, err)
					require.True(t, receipt.Accepted)
					require.True(t, receipt.Durable)
				}
				synctest.Wait()
				if kind == "runtime_report" || kind == "new_turn" {
					require.NoError(t, attempt.Err(), "runtime reports and new_turn must not interrupt")
					require.Zero(t, summaryCalls.Load())
					firstOnce.Do(func() { close(firstRelease) })
					synctest.Wait()
				} else {
					require.ErrorIs(t, context.Cause(attempt), errSteeringBoundary)
				}
				select {
				case <-summaryEntered:
				default:
					t.Fatal("compaction must run at the interrupted boundary")
				}
				require.Equal(t, int32(1), calls.Load(), "restart must await compaction")
				pending, err := h.Snapshot(t.Context())
				require.NoError(t, err)
				for _, item := range pending.MessagesSnapshot() {
					if item.Message != nil && item.Message.TurnID == requestID {
						require.True(t, item.Message.Pending)
					}
				}
				for _, message := range summaryInput {
					require.NotContains(t, message.Content, "STEERING correction")
				}
				summaryOnce.Do(func() { close(summaryRelease) })
				synctest.Wait()
				select {
				case <-restartEntered:
				default:
					t.Fatal("provider must resume after compaction")
				}
				if kind != "new_turn" {
					require.Equal(t, active.TurnID, d.ActiveRequestID())
				}
				corrections := 0
				for _, message := range restarted {
					if strings.Contains(message.Content, "STEERING correction") {
						corrections++
					}
				}
				require.Equal(t, 1, corrections)
				restartOnce.Do(func() { close(restartRelease) })
				coordinationAwait(t, h, requestID)
				coordinationSettled(t, h)
				require.Equal(t, int32(2), calls.Load(), "no repeated empty restart")
				require.Equal(t, int32(1), summaryCalls.Load())
				live, err := h.Snapshot(t.Context())
				require.NoError(t, err)
				require.Equal(t, "reserved summary", live.LastSummary())
				persisted, err := store.GetSession(t.Context(), h.ID())
				require.NoError(t, err)
				require.Equal(t, "reserved summary", persisted.LastSummary())
				require.Len(t, persisted.MessagesSnapshot(), len(live.MessagesSnapshot()))
				close(compactEvents)
				var outcomes []string
				for event := range compactEvents {
					if e, ok := event.(*SessionCompactionEvent); ok && e.Status == "completed" {
						outcomes = append(outcomes, e.Outcome)
					}
				}
				require.Equal(t, []string{CompactionOutcomeApplied}, outcomes)
			})
		})
	}
}

func TestSteeringBoundaryDuringStartupKeepsActiveRequest(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		startEntered, startRelease := make(chan struct{}), make(chan struct{})
		providerEntered, providerRelease := make(chan struct{}), make(chan struct{})
		var startOnce, providerOnce sync.Once
		var calls atomic.Int32
		var messages []chat.Message
		provider := coordinationReply("done")
		provider.call = func(ctx context.Context, input []chat.Message) (chat.MessageStream, error) {
			calls.Add(1)
			messages = input
			close(providerEntered)
			select {
			case <-providerRelease:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			return newStreamBuilder().AddContent("done").AddStopWithUsage(1, 1).Build(), nil
		}
		_, owner := coordinationRuntime(t, session.NewInMemorySessionStore(), provider, coordinationReply("unused"))
		h := coordinationCreate(t, owner.Runtime(), "startup-steering", "")
		d := h.(*sessionHandle).driver
		d.SetPreStartErrorGate(func() error { close(startEntered); <-startRelease; return nil }, nil)
		t.Cleanup(func() {
			startOnce.Do(func() { close(startRelease) })
			providerOnce.Do(func() { close(providerRelease) })
		})
		activeInput := make(chan Submission, 1)
		go func() {
			active, err := h.Submit(t.Context(), TurnInput{Content: "active", RequestID: "active"})
			require.NoError(t, err)
			activeInput <- active
		}()
		<-startEntered
		guide, err := h.Steer(t.Context(), TurnInput{Content: "startup STEERING", RequestID: "guidance"})
		require.NoError(t, err)
		startOnce.Do(func() { close(startRelease) })
		active := <-activeInput
		synctest.Wait()
		select {
		case <-providerEntered:
		default:
			t.Fatal("provider did not start")
		}
		require.Equal(t, active.TurnID, d.ActiveRequestID())
		corrections := 0
		for _, message := range messages {
			if strings.Contains(message.Content, "startup STEERING") {
				corrections++
			}
		}
		require.Equal(t, 1, corrections, "startup STEERING belongs in the first active provider request")
		coordinationAwait(t, h, guide.TurnID)
		providerOnce.Do(func() { close(providerRelease) })
		coordinationAwait(t, h, active.TurnID)
		coordinationSettled(t, h)
		require.Equal(t, int32(1), calls.Load(), "startup guidance must not become a successor turn")
	})
}

package a2a

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"iter"
	"log/slog"
	"strings"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"
	adksession "google.golang.org/adk/v2/session"
	"google.golang.org/genai"

	dagent "github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/host/turn"
	"github.com/docker/docker-agent/pkg/runtime"
	runtimeclient "github.com/docker/docker-agent/pkg/runtime/client"
	"github.com/docker/docker-agent/pkg/servesafety"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
	cgenai "github.com/docker/docker-agent/pkg/telemetry/genai"
	"github.com/docker/docker-agent/pkg/version"
)

// newDockerAgentAdapter creates a new ADK agent adapter from a docker agent team and agent name.
// When agentName is empty, the team's default agent (one explicitly named "root" if it
// exists, otherwise the first agent declared) is used.
func newDockerAgentAdapter(t *team.Team, agentName string, sessStore session.Store, safety servesafety.Resolved, workingDir string, registries ...runtime.SessionRuntime) (agent.Agent, error) {
	a, err := t.AgentOrDefault(agentName)
	if err != nil {
		return nil, fmt.Errorf("failed to get agent %s: %w", agentName, err)
	}
	agentName = a.Name()

	desc := cmp.Or(a.Description(), "Agent "+agentName)

	return agent.New(agent.Config{
		Name:        agentName,
		Description: desc,
		Run: func(ctx agent.InvocationContext) iter.Seq2[*adksession.Event, error] {
			return runDockerAgent(ctx, t, agentName, a, sessStore, safety, workingDir, registries...)
		},
	})
}

// runDockerAgent executes a docker agent and returns ADK session events
func runDockerAgent(ctx agent.InvocationContext, t *team.Team, agentName string, a *dagent.Agent, sessStore session.Store, safety servesafety.Resolved, workingDir string, registries ...runtime.SessionRuntime) iter.Seq2[*adksession.Event, error] {
	return func(yield func(*adksession.Event, error) bool) {
		// Decorate the inbound `a2a.message` SERVER span (created by
		// otelhttp.NewHandler in server.go) with the GenAI semconv
		// invoke_agent shape so dashboards can recognise A2A traffic as
		// agent invocations rather than generic JSON-RPC POSTs. The
		// runtime.session span we open below is the child that records
		// the actual work; this annotation makes the parent searchable
		// via gen_ai.operation.name="invoke_agent".
		if span := trace.SpanFromContext(ctx); span.IsRecording() {
			span.SetAttributes(
				attribute.String(cgenai.AttrOperationName, cgenai.OperationInvokeAgent),
				attribute.String(cgenai.AttrAgentName, agentName),
				attribute.String(cgenai.AttrAgentNameRuntime, agentName),
			)
		}

		// Extract user message from the ADK context
		userContent := ctx.UserContent()
		message := contentToMessage(userContent)

		// Use the A2A context ID as the docker-agent session ID so only future
		// A2A invocations with that ID can resume the conversation.
		sessionID := ctx.Session().ID()

		var sess *session.Session
		existing, err := sessStore.GetSessionByOrigin(ctx, sessionID, "a2a")
		switch {
		case err == nil:
			sess = existing
		case !errors.Is(err, session.ErrNotFound):
			yield(nil, fmt.Errorf("look up A2A session: %w", err))
			return
		default:
			_, err := sessStore.GetSession(ctx, sessionID)
			switch {
			case err == nil:
				yield(nil, errors.New("context ID is not available"))
				return
			case !errors.Is(err, session.ErrNotFound):
				yield(nil, fmt.Errorf("check A2A context ID: %w", err))
				return
			default:
				sess = session.New(
					session.WithID(sessionID),
					session.WithOrigin("a2a"),
					session.WithAgentName(agentName),
					session.WithMaxIterations(a.MaxIterations()),
					session.WithMaxConsecutiveToolCalls(a.MaxConsecutiveToolCalls()),
					session.WithMaxOldToolCallTokens(a.MaxOldToolCallTokens()),
					session.WithMaxToolResultTokens(a.MaxToolResultTokens()),
					session.WithSafetyPolicy(safety.Policy),
					session.WithNonInteractive(true),
					session.WithWorkingDir(workingDir),
				)
				sess.SetTitle("A2A Session " + sessionID)
			}
		}

		var sessionRuntime runtime.SessionRuntime
		if len(registries) != 0 {
			sessionRuntime = registries[0]
		}
		if sessionRuntime == nil {
			rt, err := runtime.NewLocalRuntime(ctx, t, runtime.WithSessionStore(sessStore), runtime.WithTracer(otel.Tracer(version.AppName)))
			if err != nil {
				yield(nil, fmt.Errorf("failed to create runtime: %w", err))
				return
			}
			supervisor := runtime.NewSessionRuntimeSupervisor(rt)
			defer func() { _ = supervisor.Shutdown(context.WithoutCancel(ctx)) }()
			sessionRuntime = supervisor.Runtime()
		}

		handle, err := sessionRuntime.SessionByID(sess.ID)
		if existing != nil {
			committed, restoreErr := runtime.RestoreSessionView(ctx, sessionRuntime, sess.ID)
			handle, err = committed.SessionHandle, restoreErr
			if err != nil {
				yield(nil, fmt.Errorf("restore A2A session: %w", err))
				return
			}
		}
		switch {
		case err != nil:
			var sessionErr *runtime.SessionError
			if !errors.As(err, &sessionErr) || sessionErr.Kind != runtime.SessionErrorNotFound {
				yield(nil, fmt.Errorf("look up canonical A2A session: %w", err))
				return
			}
			handle, err = sessionRuntime.CreateSession(ctx, sess, runtime.SessionBinding{AgentName: agentName})
		case handle.AgentName() != agentName:
			err = &runtime.SessionError{Kind: runtime.SessionErrorWrongSession, SessionID: sess.ID, Operation: runtime.SessionOperationBindAgent}
		default:
			nonInteractive := true
			_, err = handle.Edit(ctx, runtime.SessionEdit{Kind: runtime.SessionEditPolicy, SafetyCeiling: &safety.Policy, NonInteractive: &nonInteractive})
		}
		if err != nil {
			yield(nil, fmt.Errorf("bind A2A session: %w", err))
			return
		}
		ownedTurn, err := turn.Start(ctx, handle, runtime.TurnInput{Content: message})
		if err != nil {
			yield(nil, fmt.Errorf("start A2A message: %w", err))
			return
		}

		// Track accumulated content for chunked responses
		var contentBuilder strings.Builder

		// finalEvent builds the turn-complete ADK event from whatever content
		// was accumulated so far. Shared by the StreamStoppedEvent case and the
		// post-loop fallback below, so both paths build an identical event.
		finalEvent := func() *adksession.Event {
			return &adksession.Event{
				Author: agentName,
				LLMResponse: model.LLMResponse{
					Content:      genai.NewContentFromParts([]*genai.Part{{Text: contentBuilder.String()}}, genai.RoleModel),
					Partial:      false,
					TurnComplete: true,
					FinishReason: genai.FinishReasonStop,
				},
			}
		}

		// Convert docker agent events to ADK events and yield them

		termination := ownedTurn.Consume(ctx, func(_ context.Context, envelope runtime.SessionEvent) (runtimeclient.TurnDecision, error) {
			event := envelope.Event
			if ctx.Ended() {
				slog.Debug("Invocation ended, stopping agent", "agent", agentName)
				return runtimeclient.TurnTerminate, nil
			}

			switch e := event.(type) {
			case *runtime.AgentChoiceEvent:
				// Accumulate content chunks
				contentBuilder.WriteString(e.Content)

				// Create a partial response event
				adkEvent := &adksession.Event{
					Author: agentName,
					LLMResponse: model.LLMResponse{
						Content:      genai.NewContentFromParts([]*genai.Part{{Text: e.Content}}, genai.RoleModel),
						Partial:      true,
						TurnComplete: false,
					},
				}

				if !yield(adkEvent, nil) {
					return runtimeclient.TurnTerminate, nil
				}

			case *runtime.ErrorEvent:
				// Yield error and stop.
				return runtimeclient.TurnTerminate, errors.New(e.Error)

			case *runtime.StreamStoppedEvent:
				// Send final complete event with all accumulated content.
				if contentBuilder.Len() > 0 {
					yield(finalEvent(), nil)
				}
			}
			return runtimeclient.TurnContinue, nil
		})
		if termination.Err != nil {
			if errors.Is(termination.Err, context.Canceled) && ctx.Ended() {
				return
			}
			yield(nil, fmt.Errorf("observe A2A session: %w", termination.Err))
			return
		}
		if termination.Stopped || termination.Terminated {
			return
		}

		// The channel closed without a StreamStoppedEvent: the runtime bounds
		// how long it waits to deliver that event (#4136), but a consumer that
		// abandoned the channel or an unexpected close should still complete
		// the ADK turn rather than leave it hanging.
		if contentBuilder.Len() > 0 {
			yield(finalEvent(), nil)
		}
	}
}

// contentToMessage converts a genai.Content to a string message
func contentToMessage(content *genai.Content) string {
	if content == nil {
		return ""
	}

	var message string
	for _, part := range content.Parts {
		if part.Text != "" {
			if message != "" {
				message += "\n"
			}
			message += part.Text
		}
	}
	return message
}

package root

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"github.com/docker/docker-agent/pkg/api"
	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tui"
	viewhost "github.com/docker/docker-agent/pkg/tui/service/supervisor"
)

const maxReviewNewSessionTargets = 8

type reviewSessionFactory struct {
	flags      *runExecFlags
	source     config.Source
	services   runtimeServicesWithStore
	supervisor *viewhost.Supervisor
	remote     *remoteBackend
	sessions   runtime.SessionRuntime
	ctx        context.Context

	mu            sync.Mutex
	pending       map[string]*reviewSessionAttempt
	accepted      map[string]acceptedReviewSession
	acceptedOrder []string
}

type runtimeServicesWithStore interface {
	SessionStore() session.Store
}

type acceptedReviewSession struct {
	fingerprint string
	result      tui.ReviewSessionResult
}

type reviewSessionAttempt struct {
	fingerprint string
	resources   *ownedSessionResources
	sess        *session.Session
	handle      runtime.SessionHandle
	activated   bool
	registered  bool
	result      tui.ReviewSessionResult
}

func (f *runExecFlags) newReviewSessionFactory(ctx context.Context, b backend, services interface{ SessionStore() session.Store }, sessions runtime.SessionRuntime) tui.ReviewSessionFactory {
	if f.sessionReadOnly || f.sessionViewHost == nil || services == nil || sessions == nil {
		return nil
	}
	factory := &reviewSessionFactory{flags: f, services: services, sessions: sessions, supervisor: f.sessionViewHost, ctx: context.WithoutCancel(ctx), pending: make(map[string]*reviewSessionAttempt), accepted: make(map[string]acceptedReviewSession)}
	switch backend := b.(type) {
	case *localBackend:
		if backend.agentSource == nil {
			return nil
		}
		factory.source = backend.agentSource
	case *remoteBackend:
		if backend.agentFileName == "" {
			return nil
		}
		factory.remote = backend
	default:
		return nil
	}
	return factory
}

func (f *reviewSessionFactory) ListAgents(ctx context.Context) ([]tui.ReviewSessionAgent, error) {
	var configs []struct{ Name, Description string }
	if f.remote != nil {
		client, err := newRemoteClient(f.remote.flags.remoteAddress, f.remote.flags.remoteAuthTokenFile)
		if err != nil {
			return nil, err
		}
		cfg, err := client.GetAgent(ctx, f.remote.agentFileName)
		if err != nil {
			return nil, err
		}
		for _, agent := range cfg.Agents {
			configs = append(configs, struct{ Name, Description string }{Name: agent.Name, Description: agent.Description})
		}
	} else {
		cfg, err := config.Load(ctx, f.source, config.WithFlavors(f.flags.runConfig.Flavors...))
		if err != nil {
			return nil, err
		}
		for _, agent := range cfg.Agents {
			configs = append(configs, struct{ Name, Description string }{Name: agent.Name, Description: agent.Description})
		}
	}
	if len(configs) > 50 {
		configs = configs[:50]
	}
	out := make([]tui.ReviewSessionAgent, 0, len(configs))
	for _, item := range configs {
		if item.Name == "" {
			continue
		}
		var source string
		if f.remote != nil {
			source = f.remote.agentFileName
		} else {
			source = f.source.Name()
		}
		out = append(out, tui.ReviewSessionAgent{Source: source, Name: item.Name, Description: item.Description})
	}
	return out, nil
}

func (f *reviewSessionFactory) ListSessionSummaryPage(ctx context.Context, cursor string, limit int) ([]runtime.SessionSummaryEntry, string, error) {
	pager, ok := f.sessions.(runtime.SessionSummaryPager)
	if !ok {
		return nil, "", runtime.ErrUnsupported
	}
	var source string
	if f.remote != nil {
		source = f.remote.agentFileName
	} else {
		source = f.source.Name()
	}
	page, err := pager.ListSessionSummaryPage(ctx, runtime.SessionSummaryPageOptions{IncludeChildren: true, Limit: limit, Cursor: cursor, Source: source})
	return page.Entries, page.NextCursor, err
}

func (f *reviewSessionFactory) CreateAndSubmit(ctx context.Context, request tui.ReviewSessionRequest) (tui.ReviewSessionResult, error) {
	if err := validateReviewNewSessionRequest(request); err != nil {
		return tui.ReviewSessionResult{}, err
	}
	if f.remote != nil {
		if request.Source != f.remote.agentFileName {
			return tui.ReviewSessionResult{}, errors.New("review source changed")
		}
		client, err := newRemoteClient(f.remote.flags.remoteAddress, f.remote.flags.remoteAuthTokenFile)
		if err != nil {
			return tui.ReviewSessionResult{}, err
		}
		transport, err := runtime.NewSessionTransport(client, runtime.WithSessionTransportSource(f.remote.agentFileName))
		if err != nil {
			return tui.ReviewSessionResult{}, err
		}
		starter, ok := any(transport).(runtime.SessionStarter)
		if !ok {
			return tui.ReviewSessionResult{}, runtime.ErrUnsupported
		}
		submission, err := starter.StartSession(ctx, api.SessionStartRequest{
			SessionCreateRequest: api.SessionCreateRequest{
				SessionID: request.SessionID, Source: f.remote.agentFileName,
				AgentName: request.AgentName, WorkingDir: request.WorkingDir,
				ToolsApproved: f.flags.autoApprove, SafetyPolicy: f.flags.userSafetyPolicy(),
			},
			Input: api.SessionInputRequest{RequestID: request.RequestID, Content: request.Body},
		})
		if err != nil {
			return tui.ReviewSessionResult{}, err
		}
		if submission.SessionID != request.SessionID || submission.TurnID == "" {
			return tui.ReviewSessionResult{}, errors.New("remote review returned a different submission identity")
		}
		return tui.ReviewSessionResult{SessionID: submission.SessionID, TurnID: submission.TurnID, Accepted: true}, nil
	}
	if request.Source != f.source.Name() {
		return tui.ReviewSessionResult{}, errors.New("review source changed")
	}
	fingerprint, err := reviewRequestFingerprint(request)
	if err != nil {
		return tui.ReviewSessionResult{}, err
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if saved, ok := f.accepted[request.RequestID]; ok {
		if saved.fingerprint != fingerprint {
			return tui.ReviewSessionResult{}, errors.New("review request ID was reused with different content")
		}
		return saved.result, nil
	}
	attempt := f.pending[request.RequestID]
	if attempt != nil && attempt.fingerprint != fingerprint {
		return tui.ReviewSessionResult{}, errors.New("review request ID was reused with different content")
	}
	if attempt == nil {
		if len(f.pending) >= maxReviewNewSessionTargets {
			return tui.ReviewSessionResult{}, errors.New("too many unresolved review session targets")
		}
		workingDir, err := session.CaptureLocalWorkingDir(request.WorkingDir)
		if err != nil {
			return tui.ReviewSessionResult{}, err
		}
		resources, err := f.flags.buildOwnedSessionResourcesForAgent(ctx, f.source, f.services.SessionStore(), workingDir, request.AgentName)
		if err != nil {
			return tui.ReviewSessionResult{}, err
		}
		spawnReq := f.flags.createSessionRequest(workingDir)
		spawnReq.AgentName = resources.agent.Name()
		sess := session.New(append(f.flags.buildSessionOpts(resources.agent, resources.team, spawnReq), session.WithID(request.SessionID))...)
		sess.SetAttribute(sessionActorSourceAttribute, f.source.Name())
		attempt = &reviewSessionAttempt{fingerprint: fingerprint, resources: resources, sess: sess}
		f.pending[request.RequestID] = attempt
	}

	if !attempt.registered {
		identity := f.flags.freshViewOwnerIdentity(attempt.sess)
		if err := f.supervisor.RegisterSessionOwner(identity, attempt.resources.viewResources(), false); err != nil {
			attempt.resources.cleanup()
			delete(f.pending, request.RequestID)
			return tui.ReviewSessionResult{}, err
		}
		attempt.registered = true
	}
	if attempt.handle == nil {
		handle, err := attempt.resources.sessions.SessionByID(request.SessionID)
		if err != nil {
			var sessionErr *runtime.SessionError
			if !errors.As(err, &sessionErr) || sessionErr.Kind != runtime.SessionErrorNotFound {
				return tui.ReviewSessionResult{}, err
			}
			handle, err = attempt.resources.sessions.CreateSession(ctx, attempt.sess, runtime.SessionBinding{AgentName: request.AgentName})
			if err != nil {
				// Keep the same owner and ID for an idempotent recovery attempt.
				return tui.ReviewSessionResult{}, err
			}
		}
		if handle == nil || handle.ID() != request.SessionID || handle.AgentName() != request.AgentName {
			return tui.ReviewSessionResult{}, errors.New("created review session identity does not match request")
		}
		attempt.handle = handle
	}
	if !attempt.activated {
		if err := attempt.resources.activate(); err != nil {
			return tui.ReviewSessionResult{}, err
		}
		attempt.activated = true
	}
	submission, err := attempt.handle.Submit(ctx, runtime.TurnInput{Content: request.Body, RequestID: request.RequestID})
	if err != nil {
		return tui.ReviewSessionResult{}, err
	}
	if submission.SessionID != request.SessionID || submission.TurnID == "" {
		return tui.ReviewSessionResult{}, errors.New("review session returned a different submission identity")
	}
	result := tui.ReviewSessionResult{SessionID: submission.SessionID, TurnID: submission.TurnID, Accepted: true, Durable: attempt.handle.Metadata().Capabilities.Durability == "durable"}
	attempt.result = result
	delete(f.pending, request.RequestID)
	f.accepted[request.RequestID] = acceptedReviewSession{fingerprint: fingerprint, result: result}
	f.acceptedOrder = append(f.acceptedOrder, request.RequestID)
	if len(f.acceptedOrder) > 128 {
		oldest := f.acceptedOrder[0]
		f.acceptedOrder = f.acceptedOrder[1:]
		delete(f.accepted, oldest)
	}
	return result, nil
}

func validateReviewNewSessionRequest(request tui.ReviewSessionRequest) error {
	if request.SessionID == "" || request.RequestID == "" || request.AgentName == "" || request.WorkingDir == "" || request.Source == "" || request.Body == "" {
		return errors.New("review new-session request is incomplete")
	}
	return nil
}

func reviewRequestFingerprint(request tui.ReviewSessionRequest) (string, error) {
	data, err := json.Marshal(request)
	if err != nil {
		return "", fmt.Errorf("encoding review request: %w", err)
	}
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:]), nil
}

var _ tui.ReviewSessionFactory = (*reviewSessionFactory)(nil)

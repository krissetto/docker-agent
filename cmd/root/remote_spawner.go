package root

import (
	"context"
	"errors"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tui"
	viewhost "github.com/docker/docker-agent/pkg/tui/service/supervisor"
)

func (b *remoteBackend) remoteSpawner(services app.Services, sessions runtime.SessionRuntime) tui.SessionSpawner {
	if b == nil || b.flags == nil || services == nil || sessions == nil {
		return nil
	}
	return func(ctx context.Context, workingDir string) (tui.SpawnedSession, error) {
		// Workspace values are opaque server paths. Never stat, normalize against
		// the client cwd, or chdir into them.
		if workingDir == "" {
			workingDir = b.flags.remoteWorkingDir
		}
		req := b.flags.createSessionRequest(workingDir)
		sess := session.New(session.WithAgentName(services.CurrentAgentInfo(ctx).Name), session.WithWorkingDir(workingDir), session.WithToolsApproved(req.ToolsApproved), session.WithSafetyPolicy(req.SafetyPolicy))
		if p := req.GlobalPermissions; p != nil && !p.IsEmpty() {
			sess.Permissions = &session.PermissionsConfig{Allow: p.AllowPatterns(), Ask: p.AskPatterns(), Deny: p.DenyPatterns()}
		}
		handle, err := sessions.CreateSession(ctx, sess, runtime.SessionBinding{AgentName: sess.AgentName, Model: b.model})
		if err != nil {
			return tui.SpawnedSession{}, err
		}
		return b.remoteAttachedApp(ctx, services, sessions, handle)
	}
}

func (b *remoteBackend) Restorer(services app.Services, sessions runtime.SessionRuntime) tui.SessionRestorer {
	return func(ctx context.Context, id, _ string) (tui.SpawnedSession, error) {
		if b == nil || b.flags == nil || services == nil || sessions == nil {
			return tui.SpawnedSession{}, runtime.ErrUnsupported
		}
		if id == "" {
			return tui.SpawnedSession{}, errors.New("cannot restore an empty remote session ID")
		}
		handle, err := sessions.SessionByID(id)
		if err != nil {
			return tui.SpawnedSession{}, err
		}
		if hydrator, ok := handle.(runtime.Hydrator); ok {
			if err := hydrator.Hydrate(ctx); err != nil {
				return tui.SpawnedSession{}, err
			}
		}
		return b.remoteAttachedApp(ctx, services, sessions, handle)
	}
}

func (b *remoteBackend) remoteAttachedApp(ctx context.Context, services app.Services, sessions runtime.SessionRuntime, handle runtime.SessionHandle) (tui.SpawnedSession, error) {
	sess, err := handle.Snapshot(ctx)
	if err != nil {
		return tui.SpawnedSession{}, err
	}
	if sess == nil || sess.ID != handle.ID() {
		return tui.SpawnedSession{}, errors.New("invalid remote session snapshot")
	}
	if sess.AttributesSnapshot()[sessionActorSourceAttribute] != b.agentFileName {
		return tui.SpawnedSession{}, errors.New("remote session belongs to a different source")
	}
	if sess.ParentID != "" {
		return tui.SpawnedSession{}, errors.New("child sessions require confirmed session view attachment")
	}
	sess.AgentName = handle.AgentName()
	opts := []app.Opt{app.WithRuntimeServices(services)}
	if b.flags.sessionReadOnly {
		opts = append(opts, app.WithReadOnly())
	}
	if b.flags.sessionViewHost != nil {
		resources := viewhost.ViewOwnerResources{Services: services, Sessions: sessions, NewApp: resolvedViewBuilder(sessions, opts)}
		if err := b.flags.sessionViewHost.RegisterSessionOwner(b.flags.freshViewOwnerIdentity(sess), resources, false); err != nil {
			return tui.SpawnedSession{}, err
		}
	}
	binding := runtime.SessionBinding{AgentName: handle.AgentName(), Model: handle.Metadata().Model}
	a, err := app.NewResolved(ctx, sessions, runtime.CommittedSessionView{SessionHandle: handle, Info: runtime.PreparedSessionViewInfo{SessionID: sess.ID, RootSessionID: sess.ID, Session: sess, WorkingDir: sess.WorkingDir, Binding: binding}}, opts...)
	if err != nil {
		return tui.SpawnedSession{}, err
	}
	return tui.SpawnedSession{App: a, Session: sess, Ownership: tui.RuntimeBorrowed}, nil
}

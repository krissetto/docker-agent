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
	transport, ok := sessions.(*runtime.SessionTransport)
	if !ok {
		return nil
	}
	return func(ctx context.Context, workingDir string) (tui.SpawnedSession, error) {
		// Workspace values are opaque server paths. Never stat, normalize against
		// the client cwd, or chdir into them.
		if workingDir == "" {
			workingDir = b.flags.remoteWorkingDir
		}
		req := b.flags.createSessionRequest(workingDir)
		// The Kit owns both ends of this connection; let its workspace runtime
		// choose the default from the current team generation. Standalone remote
		// clients can still talk to older servers that require an explicit name.
		var agentName, model string
		if !b.flags.managedAPI {
			remote, ok := services.(*runtime.RemoteServices)
			if !ok {
				return tui.SpawnedSession{}, runtime.ErrUnsupported
			}
			var err error
			agentName, err = remote.DefaultAgentName(ctx)
			if err != nil {
				return tui.SpawnedSession{}, err
			}
			if len(b.flags.modelOverrides) != 0 && b.handle != nil && b.handle.AgentName() == agentName {
				model = b.model
			}
		}
		sess := session.New(session.WithAgentName(agentName), session.WithWorkingDir(workingDir), session.WithToolsApproved(req.ToolsApproved), session.WithSafetyPolicy(req.SafetyPolicy))
		if p := req.GlobalPermissions; p != nil && !p.IsEmpty() {
			sess.Permissions = &session.PermissionsConfig{Allow: p.AllowPatterns(), Ask: p.AskPatterns(), Deny: p.DenyPatterns()}
		}
		handle, created, err := transport.CreateSessionView(ctx, sess, runtime.SessionBinding{AgentName: agentName, Model: model})
		if err != nil {
			return tui.SpawnedSession{}, err
		}
		return b.remoteCreatedApp(ctx, services, sessions, handle, created)
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
		reader, ok := sessions.(runtime.SessionViewInfoReader)
		if !ok {
			return tui.SpawnedSession{}, runtime.UnsupportedSessionOperation(id, "prepare_view")
		}
		info, err := reader.ConfirmedSessionViewInfo(ctx, id)
		if err != nil {
			return tui.SpawnedSession{}, err
		}
		if info.Session == nil || info.Session.AttributesSnapshot()[sessionActorSourceAttribute] != b.agentFileName {
			return tui.SpawnedSession{}, errors.New("remote session belongs to a different source")
		}
		if b.flags.sessionViewHost != nil {
			application, err := b.flags.restoreHostedSession(ctx, id)
			if err != nil {
				return tui.SpawnedSession{}, err
			}
			return tui.SpawnedSession{App: application, Session: application.Session(), Ownership: tui.RuntimeBorrowed}, nil
		}
		committed, err := runtime.RestoreSessionView(ctx, sessions, id)
		if err != nil {
			return tui.SpawnedSession{}, err
		}
		opts := withTitleGenerator(ctx, services, []app.Opt{app.WithRuntimeServices(services)})
		if b.flags.sessionReadOnly {
			opts = append(opts, app.WithReadOnly())
		}
		if committed.Info.Attach != nil {
			opts = append(opts, app.WithSubagentAttach(*committed.Info.Attach))
		}
		application, err := app.NewResolved(ctx, sessions, committed, opts...)
		if err != nil {
			return tui.SpawnedSession{}, err
		}
		return tui.SpawnedSession{App: application, Session: application.Session(), Ownership: tui.RuntimeBorrowed}, nil
	}
}

func (b *remoteBackend) remoteCreatedApp(ctx context.Context, services app.Services, sessions runtime.SessionRuntime, handle runtime.SessionHandle, sess *session.Session) (tui.SpawnedSession, error) {
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
	opts := withTitleGenerator(ctx, services, []app.Opt{app.WithRuntimeServices(services)})
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

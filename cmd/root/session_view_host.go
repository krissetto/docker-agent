package root

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tui"
	viewhost "github.com/docker/docker-agent/pkg/tui/service/supervisor"
)

const sessionActorSourceAttribute = "docker-agent.actor.source"

// configureSessionViewHost issues one scope for the resolved backend's source,
// configuration and authorization lifetime. Both UIs and --listen receive this
// same Supervisor; this adapter owns no session or runtime registry.
func (f *runExecFlags) configureSessionViewHost(ctx context.Context, b backend, services app.Services, sessions runtime.SessionRuntime, initial *session.Session, spawner tui.SessionSpawner, cleanup func()) error {
	f.sessionViewScope = viewhost.NewViewOwnerScope()
	f.sessionViewStore = services.SessionStore()
	f.sessionViewWorkingDir = initial.WorkingDir
	f.sessionViewHost = viewhost.New(spawner)
	var source config.Source
	switch b := b.(type) {
	case *localBackend:
		source = b.agentSource
		if source != nil {
			f.sessionViewSource = source.Name()
		}
	case *remoteBackend:
		// The opaque scope separates authorization/config lifetimes; retain
		// the exact address/ref rather than stripping URL query identity.
		f.sessionViewSource = b.flags.remoteAddress + "\x00" + b.agentFileName
	}
	resolve := localViewOwnerResolver(f.sessionViewScope, f.sessionViewSource, services.SessionStore())
	var factory func(context.Context, viewhost.ViewOwnerIdentity) (viewhost.ViewOwnerResources, error)
	if _, local := b.(*localBackend); local {
		factory = func(factoryCtx context.Context, identity viewhost.ViewOwnerIdentity) (viewhost.ViewOwnerResources, error) {
			if identity.Scope != f.sessionViewScope || identity.Source != f.sessionViewSource {
				return viewhost.ViewOwnerResources{}, viewOwnerError(identity.RootSessionID, "view_source", "source lifetime does not match this host")
			}
			if identity.RootBinding.AgentName == "" {
				return viewhost.ViewOwnerResources{}, viewOwnerError(identity.RootSessionID, "view_binding", "root has no persisted agent binding")
			}
			if identity.RootWorkingDir == "" || !filepath.IsAbs(identity.RootWorkingDir) {
				return viewhost.ViewOwnerResources{}, session.ErrWorkingDirUnavailable
			}
			info, err := os.Stat(identity.RootWorkingDir)
			if err != nil {
				return viewhost.ViewOwnerResources{}, fmt.Errorf("session view workspace: %w", err)
			}
			if !info.IsDir() {
				return viewhost.ViewOwnerResources{}, errors.New("session view workspace is not a directory")
			}
			resources, err := f.buildOwnedSessionResources(factoryCtx, source, services.SessionStore(), identity.RootWorkingDir)
			if err != nil {
				return viewhost.ViewOwnerResources{}, err
			}
			if _, err := resources.team.Agent(identity.RootBinding.AgentName); err != nil {
				resources.cleanup()
				return viewhost.ViewOwnerResources{}, err
			}
			return resources.viewResources(), nil
		}
	} else {
		resolve = remoteViewOwnerResolver(f.sessionViewScope, f.sessionViewSource, sessions)
	}
	if err := f.sessionViewHost.ConfigureSessionViews(ctx, viewhost.HostViewConfig{
		Resolve:               resolve,
		Factory:               factory,
		MaxRetainedViewOwners: viewhost.DefaultMaxRetainedViewOwners,
	}); err != nil {
		return err
	}
	identity := f.freshViewOwnerIdentity(initial)
	resources := viewhost.ViewOwnerResources{Services: services, Sessions: sessions, Cleanup: cleanup}
	opts := withTitleGenerator(ctx, services, []app.Opt{app.WithRuntimeServices(services)})
	if f.snapshotController != nil {
		opts = append(opts, app.WithSnapshotController(f.snapshotController))
	}
	if f.sessionReadOnly {
		opts = append(opts, app.WithReadOnly())
	}
	resources.NewApp = resolvedViewBuilder(sessions, opts)
	return f.sessionViewHost.RegisterSessionOwner(identity, resources, true)
}

func (f *runExecFlags) restoreHostedSession(ctx context.Context, sessionID string) (*app.App, error) {
	var application *app.App
	err := f.sessionViewHost.WithSessionOwner(ctx, sessionID, func(ctx context.Context, resources viewhost.ViewOwnerResources) error {
		handle, err := resources.Sessions.SessionByID(sessionID)
		if err != nil {
			var sessionErr *runtime.SessionError
			if !errors.As(err, &sessionErr) || sessionErr.Kind != runtime.SessionErrorNotFound {
				return err
			}
			sess, err := resources.Services.SessionStore().GetSession(ctx, sessionID)
			if err != nil {
				return err
			}
			if sess == nil || sess.ID != sessionID {
				return viewOwnerError(sessionID, "restore_identity", "store returned a different session")
			}
			if sess.ParentID != "" {
				return viewOwnerError(sessionID, "restore_binding", "child sessions require confirmed view acquisition")
			}
			if sess.AgentName == "" {
				sess.AgentName = sess.AttributesSnapshot()[runtime.SessionAgentAttribute]
			}
			handle, err = resources.Sessions.CreateSession(ctx, sess, runtime.SessionBinding{AgentName: sess.AgentName, Model: sess.AgentModelOverrides[sess.AgentName]})
			if err != nil {
				return err
			}
			// Ordinary root restore historically rehydrated and woke its
			// subtree in App.Start. NewResolved skips that work deliberately
			// for dormant views, so preserve it here only for a cold root.
			if restorer, ok := resources.Sessions.(runtime.TreeRestorer); ok {
				if err := restorer.RestoreSessionTree(ctx, sess); err != nil {
					return err
				}
			}
		}
		sess, err := handle.Snapshot(ctx)
		if err != nil {
			return err
		}
		metadata := handle.Metadata()
		committed := runtime.CommittedSessionView{SessionHandle: handle, Info: runtime.PreparedSessionViewInfo{
			SessionID: sessionID, RootSessionID: sessionID, Session: sess, WorkingDir: sess.WorkingDir,
			Binding: runtime.SessionBinding{AgentName: handle.AgentName(), Model: metadata.Model},
		}}
		application, err = resources.NewApp(ctx, committed)
		return err
	})
	return application, err
}

func resolvedViewBuilder(sessions runtime.SessionRuntime, opts []app.Opt) func(context.Context, runtime.CommittedSessionView) (*app.App, error) {
	return func(ctx context.Context, committed runtime.CommittedSessionView) (*app.App, error) {
		viewOpts := append([]app.Opt(nil), opts...)
		if committed.Info.Attach != nil {
			viewOpts = append(viewOpts, app.WithSubagentAttach(*committed.Info.Attach))
		}
		return app.NewResolved(ctx, sessions, committed, viewOpts...)
	}
}

func (r *ownedSessionResources) viewResources() viewhost.ViewOwnerResources {
	return viewhost.ViewOwnerResources{
		Services: r.services,
		Sessions: r.sessions,
		NewApp:   resolvedViewBuilder(r.sessions, r.opts),
		Cleanup:  r.cleanup,
		Retain:   r.activate,
	}
}

func (f *runExecFlags) freshViewOwnerIdentity(sess *session.Session) viewhost.ViewOwnerIdentity {
	return viewhost.ViewOwnerIdentity{
		Scope:          f.sessionViewScope,
		Source:         f.sessionViewSource,
		RootSessionID:  sess.ID,
		RootWorkingDir: sess.WorkingDir,
		RootBinding:    runtime.SessionBinding{AgentName: sess.AgentName},
	}
}

func viewOwnerError(id, operation, detail string) error {
	return &runtime.SessionError{Kind: runtime.SessionErrorInvalid, SessionID: id, Operation: runtime.SessionOperation(operation), Detail: detail}
}

// localViewOwnerResolver is a confirmed, read-only ancestry walk. Catalog/UI
// metadata never selects the source, authorization scope, root agent or cwd.
func localViewOwnerResolver(scope *viewhost.ViewOwnerScope, source string, store session.Store) func(context.Context, string) (viewhost.ViewOwnerIdentity, error) {
	return func(ctx context.Context, id string) (viewhost.ViewOwnerIdentity, error) {
		if store == nil {
			return viewhost.ViewOwnerIdentity{}, runtime.ErrUnsupported
		}
		selected, err := store.GetSession(ctx, id)
		if err != nil {
			return viewhost.ViewOwnerIdentity{}, err
		}
		if selected == nil || selected.ID != id {
			return viewhost.ViewOwnerIdentity{}, viewOwnerError(id, "view_identity", "store returned a different session")
		}
		root := selected
		seen := map[string]bool{}
		for depth := 0; ; depth++ {
			if err := ctx.Err(); err != nil {
				return viewhost.ViewOwnerIdentity{}, err
			}
			if depth >= 32 || seen[root.ID] {
				return viewhost.ViewOwnerIdentity{}, viewOwnerError(id, "view_ancestry", "invalid root ancestry")
			}
			seen[root.ID] = true
			attrs := root.AttributesSnapshot()
			if recorded := attrs[sessionActorSourceAttribute]; recorded != "" && recorded != source {
				return viewhost.ViewOwnerIdentity{}, viewOwnerError(id, "view_source", "persisted source does not match this host")
			}
			if root.ParentID == "" {
				break
			}
			parent, err := store.GetSession(ctx, root.ParentID)
			if err != nil {
				return viewhost.ViewOwnerIdentity{}, err
			}
			if parent == nil || parent.ID != root.ParentID {
				return viewhost.ViewOwnerIdentity{}, viewOwnerError(id, "view_ancestry", "store returned a different parent")
			}
			if recorded := attrs[sessionActorSourceAttribute]; recorded != "" && recorded != parent.AttributesSnapshot()[sessionActorSourceAttribute] {
				return viewhost.ViewOwnerIdentity{}, viewOwnerError(id, "view_source", "source differs across ancestry")
			}
			root = parent
		}
		bound := root.AttributesSnapshot()[runtime.SessionAgentAttribute]
		return viewhost.ViewOwnerIdentity{Scope: scope, Source: source, RootSessionID: root.ID, RootWorkingDir: root.WorkingDir, RootBinding: runtime.SessionBinding{AgentName: bound}}, nil
	}
}

func remoteViewOwnerResolver(scope *viewhost.ViewOwnerScope, source string, sessions runtime.SessionRuntime) func(context.Context, string) (viewhost.ViewOwnerIdentity, error) {
	return func(ctx context.Context, id string) (viewhost.ViewOwnerIdentity, error) {
		reader, ok := sessions.(runtime.SessionViewInfoReader)
		if !ok {
			return viewhost.ViewOwnerIdentity{}, runtime.ErrUnsupported
		}
		info, err := reader.ConfirmedSessionViewInfo(ctx, id)
		if err != nil {
			return viewhost.ViewOwnerIdentity{}, err
		}
		root := info
		if info.RootSessionID != id {
			root, err = reader.ConfirmedSessionViewInfo(ctx, info.RootSessionID)
			if err != nil {
				return viewhost.ViewOwnerIdentity{}, err
			}
		}
		return viewhost.ViewOwnerIdentity{Scope: scope, Source: source, RootSessionID: root.SessionID, RootWorkingDir: root.WorkingDir, RootBinding: root.Binding}, nil
	}
}

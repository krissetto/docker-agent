package tui

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"

	tea "charm.land/bubbletea/v2"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tui/components/notification"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/page/chat"
	"github.com/docker/docker-agent/pkg/tui/service/supervisor"
)

var errHostedChild = errors.New("child session requires prepared view acquisition")

// loadHostedRoot retains ordinary root restoration semantics inside the same
// host admission fence as prepared views. A live winner is never reconciled or
// tree-restored again. Child sessions use the prepared view path instead.
type hostedSessionData struct {
	committed runtime.CommittedSessionView
	newApp    func(context.Context, runtime.CommittedSessionView) (*app.App, error)
	prepared  supervisor.PreparedHostedView
}

func loadHostedRoot(ctx context.Context, owner *supervisor.Supervisor, id string) (*hostedSessionData, error) {
	var data *hostedSessionData
	err := owner.WithSessionOwner(ctx, id, func(ctx context.Context, resources supervisor.ViewOwnerResources) error {
		handle, err := resources.Sessions.SessionByID(id)
		if err != nil {
			var sessionErr *runtime.SessionError
			if !errors.As(err, &sessionErr) || sessionErr.Kind != runtime.SessionErrorNotFound {
				return err
			}
			if resources.Services == nil || resources.Services.SessionStore() == nil {
				return runtime.ErrUnsupported
			}
			sess, err := resources.Services.SessionStore().GetSession(ctx, id)
			if err != nil {
				return err
			}
			if sess == nil || sess.ID != id {
				return errors.New("stored session identity changed")
			}
			if sess.ParentID != "" {
				return errHostedChild
			}
			if sess.AgentName == "" {
				sess.AgentName = sess.AttributesSnapshot()[runtime.SessionAgentAttribute]
			}
			handle, err = resources.Sessions.CreateSession(ctx, sess, runtime.SessionBinding{AgentName: sess.AgentName, Model: sess.AgentModelOverrides[sess.AgentName]})
			if err != nil {
				return err
			}
			if restorer, ok := resources.Sessions.(runtime.TreeRestorer); ok {
				if err := restorer.RestoreSessionTree(ctx, sess); err != nil {
					return err
				}
			}
		}
		sess, err := handle.Snapshot(ctx)
		if errors.Is(err, runtime.ErrUnsupported) && resources.Services != nil && resources.Services.SessionStore() != nil {
			sess, err = resources.Services.SessionStore().GetSession(ctx, id)
		}
		if err != nil {
			return err
		}
		if sess == nil || sess.ID != id || handle.ID() != id {
			return errors.New("canonical snapshot identity changed")
		}
		if sess.ParentID != "" {
			return errHostedChild
		}
		metadata := handle.Metadata()
		committed := runtime.CommittedSessionView{SessionHandle: handle, Info: runtime.PreparedSessionViewInfo{SessionID: id, RootSessionID: id, Session: sess, WorkingDir: sess.WorkingDir, Binding: runtime.SessionBinding{AgentName: handle.AgentName(), Model: metadata.Model}}}
		data = &hostedSessionData{committed: committed, newApp: resources.NewApp}
		return nil
	})
	return data, err
}

type hostedLoadRequest struct {
	application                       *app.App
	targetApp                         *app.App
	oldPersisted                      string
	focus, target, persisted, pending string
	generation                        uint64
	targetGeneration                  uint64
	cancel                            context.CancelFunc
	sidebar                           chat.SidebarSettings
	send                              *messages.SendMsg
}

type hostedLoadResult struct {
	request *hostedLoadRequest
	data    *hostedSessionData
	err     error
}

func (m *appModel) beginHostedLoad(id, target string, send *messages.SendMsg) tea.Cmd {
	if m.hostedLoad != nil {
		m.hostedLoad.cancel()
	}
	generation, _ := m.supervisor.RouteGeneration(m.paneFocus())
	ctx, cancel := context.WithCancel(m.ctx())
	request := &hostedLoadRequest{application: m.application, focus: m.paneFocus(), target: target, persisted: id, generation: generation, cancel: cancel, sidebar: m.chatPage.GetSidebarSettings(), send: send}
	if target != "" {
		request.pending = m.pendingRestores[target]
		request.oldPersisted = m.persistedSessionID(target)
		request.targetGeneration, _ = m.supervisor.RouteGeneration(target)
		if runner := m.supervisor.GetRunner(target); runner != nil {
			request.targetApp = runner.App
		}
	}
	m.hostedLoad = request
	owner := m.supervisor
	services, sessions := m.application.Runtime(), m.application.SessionRuntime()
	resolve := m.compatResolve
	originDir := ""
	if runner := m.supervisor.GetRunner(request.focus); runner != nil {
		originDir = runner.WorkingDir
	}
	origin, placeholder := m.application, request.targetApp
	presentationOnly := m.legacyPresentationOnly && sessions == nil
	return func() tea.Msg {
		if presentationOnly {
			if services == nil || services.SessionStore() == nil {
				return hostedLoadResult{request: request, err: runtime.ErrUnsupported}
			}
			snapshot, err := services.SessionStore().GetSession(ctx, id)
			if err != nil {
				return hostedLoadResult{request: request, err: err}
			}
			if snapshot == nil || snapshot.ID != id {
				return hostedLoadResult{request: request, err: errors.New("stored display session identity changed")}
			}
			data := &hostedSessionData{committed: runtime.CommittedSessionView{Info: runtime.PreparedSessionViewInfo{SessionID: id, Session: snapshot, Binding: runtime.SessionBinding{AgentName: snapshot.AgentName, Model: snapshot.AgentModelOverrides[snapshot.AgentName]}}}, newApp: func(viewCtx context.Context, view runtime.CommittedSessionView) (*app.App, error) {
				return app.New(viewCtx, nil, view.Info.Session, view.Info.Binding, app.WithRuntimeServices(services)), nil
			}}
			return hostedLoadResult{request: request, data: data}
		}
		child, err := hostedSelectionIsChild(ctx, services, sessions, id)
		var data *hostedSessionData
		if err == nil && child {
			prepared, prepareErr := owner.AcquireSessionView(ctx, id)
			err = prepareErr
			if err == nil {
				committed, commitErr := prepared.Commit(ctx)
				err = commitErr
				if err == nil {
					data = &hostedSessionData{committed: committed, newApp: prepared.NewApp, prepared: prepared}
				} else {
					prepared.Abort()
				}
			}
		} else if err == nil {
			if resolve != nil {
				err = registerCompatibilityRoot(ctx, owner, resolve, origin, placeholder, originDir, id)
			}
			if err == nil {
				data, err = loadHostedRoot(ctx, owner, id)
			}
		}
		if ctx.Err() != nil && data != nil && data.prepared != nil {
			data.prepared.Abort()
			data = nil
			err = ctx.Err()
		}
		return hostedLoadResult{request: request, data: data, err: err}
	}
}

func (m *appModel) finishHostedLoad(result hostedLoadResult) tea.Cmd {
	request := result.request
	closeResult := func() {
		if result.data != nil && result.data.prepared != nil {
			result.data.prepared.Abort()
		}
	}
	if request == nil || m.hostedLoad != request {
		closeResult()
		return nil
	}
	m.hostedLoad = nil
	defer request.cancel()
	generation, exists := m.supervisor.RouteGeneration(request.focus)
	if request.target != "" {
		targetGeneration, targetExists := m.supervisor.RouteGeneration(request.target)
		runner := m.supervisor.GetRunner(request.target)
		if !targetExists || targetGeneration != request.targetGeneration || runner == nil || runner.App != request.targetApp {
			closeResult()
			return nil
		}
	}
	if !exists || generation != request.generation || m.paneFocus() != request.focus || m.application != request.application || (request.target != "" && m.pendingRestores[request.target] != request.pending) {
		closeResult()
		return nil
	}
	if result.err != nil || result.data == nil {
		closeResult()
		return notification.ErrorCmd(fmt.Sprintf("Cannot load session; current view unchanged: %v", result.err))
	}
	application, err := result.data.newApp(m.ctx(), result.data.committed)
	if err != nil {
		closeResult()
		return notification.ErrorCmd("Cannot attach loaded session: " + err.Error())
	}
	defer closeResult()
	sess := application.Session()
	if sess == nil || sess.ID != request.persisted {
		application.Close()
		return notification.ErrorCmd("Loaded session identity changed")
	}
	if existing := m.supervisor.FindBySession(sess.ID); existing != nil && existing.ID != request.target {
		application.Close()
		_, cmd := m.handleSwitchTab(existing.ID)
		return cmd
	}
	id := request.target
	if id == "" {
		var err error
		id, err = m.supervisor.AddSession(m.ctx(), application, sess, sess.WorkingDir, nil)
		if err != nil {
			application.Close()
			return notification.ErrorCmd(err.Error())
		}
	} else {
		if editor := m.editors[id]; editor != nil {
			editor.Cleanup()
		}
		m.supervisor.ReplaceRunnerApp(m.ctx(), id, supervisor.SpawnedSession{App: application, Session: sess, Ownership: supervisor.RuntimeBorrowed}, sess.WorkingDir)
	}
	m.createSessionComponents(id, application, sess)
	m.chatPages[id].SetSidebarSettings(request.sidebar)
	delete(m.pendingRestores, id)
	sidebarCmd := m.applySidebarCollapsed(id)
	var persistenceWarning tea.Cmd
	if m.tuiStore != nil {
		var persistErr error
		if request.target != "" {
			persistErr = m.tuiStore.UpdateTabSessionID(m.ctx(), request.oldPersisted, sess.ID)
		} else {
			persistErr = m.tuiStore.AddTab(m.ctx(), sess.ID, sess.WorkingDir)
		}
		if persistErr != nil {
			persistenceWarning = notification.WarningCmd("Session opened but tab persistence failed: " + persistErr.Error())
		}
	}
	_, focusCmd := m.handleSwitchTab(id)
	initCmd := m.routePaneCmd(id, tea.Batch(m.chatPages[id].Init(), chat.WatchGitBranch(m.chatPages[id]), m.editors[id].Init()))
	if request.send != nil {
		generation, _ := m.supervisor.RouteGeneration(id)
		return tea.Sequence(tea.Batch(initCmd, focusCmd, sidebarCmd, persistenceWarning), stampSendCommand(id, generation, func() tea.Msg { return *request.send }))
	}
	return tea.Batch(initCmd, focusCmd, sidebarCmd, persistenceWarning)
}

// Classification is read-only and precedes ordinary owner admission, so a child
// cannot turn an optional retained view into uncapped ordinary safety ownership.
func hostedSelectionIsChild(ctx context.Context, services app.Services, sessions runtime.SessionRuntime, id string) (bool, error) {
	if inspector, ok := sessions.(interface {
		ConfirmedSessionViewInfo(ctx context.Context, id string) (runtime.PreparedSessionViewInfo, error)
	}); ok {
		info, err := inspector.ConfirmedSessionViewInfo(ctx, id)
		if err != nil {
			return false, err
		}
		if info.Session == nil || info.SessionID != id {
			return false, errors.New("selected session identity unavailable")
		}
		return info.Session.ParentID != "", nil
	}
	if services != nil && services.SessionStore() != nil {
		sess, err := services.SessionStore().GetSession(ctx, id)
		if err != nil {
			return false, err
		}
		if sess == nil || sess.ID != id {
			return false, errors.New("selected session identity unavailable")
		}
		return sess.ParentID != "", nil
	}
	if catalog, ok := sessions.(runtime.SessionSummaryCatalog); ok {
		rows, err := catalog.ListSessionSummaries(ctx, runtime.SessionSummaryOptions{IncludeChildren: true})
		if err != nil {
			return false, err
		}
		for _, row := range rows {
			if row.SessionID == id {
				return row.ParentID != "", nil
			}
		}
	}
	return false, errors.New("runtime cannot classify the selected session without admission")
}

// configureCompatibilityHost preserves legacy canonical A/B operations on the
// same supervisor. It deliberately installs no private foreign-view factory.
func (m *appModel) configureCompatibilityHost(ctx context.Context, initial *app.App, cwd string) error {
	if initial.SessionRuntime() == nil {
		return nil
	}
	scope := supervisor.NewViewOwnerScope()
	m.compatScope = scope
	services, sessions := initial.Runtime(), initial.SessionRuntime()
	resolve := func(ctx context.Context, id string) (supervisor.ViewOwnerIdentity, error) {
		read := func(selected string) (*session.Session, error) {
			handle, lookupErr := sessions.SessionByID(selected)
			if lookupErr == nil && handle != nil {
				snapshot, snapshotErr := handle.Snapshot(ctx)
				if snapshotErr == nil {
					return snapshot, nil
				}
				if !errors.Is(snapshotErr, runtime.ErrUnsupported) {
					return nil, snapshotErr
				}
			} else if lookupErr != nil {
				var missing *runtime.SessionError
				if !errors.As(lookupErr, &missing) || missing.Kind != runtime.SessionErrorNotFound {
					return nil, lookupErr
				}
			}
			if services == nil || services.SessionStore() == nil {
				return nil, runtime.ErrUnsupported
			}
			return services.SessionStore().GetSession(ctx, selected)
		}
		snapshot, err := read(id)
		if err != nil {
			return supervisor.ViewOwnerIdentity{}, err
		}
		seen := map[string]bool{}
		for snapshot != nil && snapshot.ParentID != "" {
			if seen[snapshot.ID] {
				return supervisor.ViewOwnerIdentity{}, errors.New("cyclic session ancestry")
			}
			seen[snapshot.ID] = true
			childSource := snapshot.AttributesSnapshot()["docker-agent.actor.source"]
			parent, err := read(snapshot.ParentID)
			if err != nil {
				return supervisor.ViewOwnerIdentity{}, err
			}
			if parent == nil || (childSource != "" && childSource != parent.AttributesSnapshot()["docker-agent.actor.source"]) {
				return supervisor.ViewOwnerIdentity{}, errors.New("session ancestry source mismatch")
			}
			snapshot = parent
		}
		if snapshot == nil || snapshot.ID == "" {
			return supervisor.ViewOwnerIdentity{}, errors.New("session root identity unavailable")
		}
		binding := runtime.SessionBinding{AgentName: snapshot.AgentName}
		if binding.AgentName == "" {
			binding.AgentName = snapshot.AttributesSnapshot()[runtime.SessionAgentAttribute]
		}
		return supervisor.ViewOwnerIdentity{Scope: scope, Source: "embedded", RootSessionID: snapshot.ID, RootWorkingDir: snapshot.WorkingDir, RootBinding: binding}, nil
	}
	m.compatResolve = resolve
	if err := m.supervisor.ConfigureSessionViews(ctx, supervisor.HostViewConfig{Resolve: resolve, MaxRetainedViewOwners: supervisor.DefaultMaxRetainedViewOwners}); err != nil {
		return err
	}
	identity := supervisor.ViewOwnerIdentity{Scope: scope, Source: "embedded", RootSessionID: initial.Session().ID, RootWorkingDir: cwd, RootBinding: initial.Binding()}
	return m.supervisor.RegisterSessionOwner(identity, compatibilityResources(initial, nil), true)
}

func compatibilityResources(application *app.App, cleanup func()) supervisor.ViewOwnerResources {
	services, sessions := application.Runtime(), application.SessionRuntime()
	return supervisor.ViewOwnerResources{Services: services, Sessions: sessions, Cleanup: cleanup, NewApp: func(ctx context.Context, view runtime.CommittedSessionView) (*app.App, error) {
		return app.NewResolvedFromTemplate(ctx, sessions, view, application)
	}}
}

// registerCompatibilityRoot is ordinary-load-only. A legacy spawner is never
// installed as HostViewConfig.Factory and is never called by archived splits.
func registerCompatibilityRoot(ctx context.Context, owner *supervisor.Supervisor, resolve func(context.Context, string) (supervisor.ViewOwnerIdentity, error), origin, placeholder *app.App, originDir, id string) error {
	identity, err := resolve(ctx, id)
	if err != nil {
		return err
	}
	// Discover every registered/hidden/retained owner before asking the public
	// ordinary spawner for another blank runtime. Only the exact no-factory
	// capability result permits this compatibility path; other errors are final.
	err = owner.WithSessionOwner(ctx, id, func(context.Context, supervisor.ViewOwnerResources) error { return nil })
	if err == nil {
		return nil
	}
	var unavailable *runtime.SessionError
	if !errors.As(err, &unavailable) || unavailable.Kind != runtime.SessionErrorUnsupported || unavailable.Operation != "host_view" || unavailable.Detail != "foreign workspace acquisition unavailable" {
		return err
	}
	if handle, lookupErr := origin.SessionRuntime().SessionByID(id); lookupErr == nil && handle != nil {
		return owner.RegisterSessionOwner(identity, compatibilityResources(origin, nil), false)
	} else if lookupErr != nil {
		var missing *runtime.SessionError
		if !errors.As(lookupErr, &missing) || missing.Kind != runtime.SessionErrorNotFound {
			return lookupErr
		}
	}
	application := origin
	var cleanup func()
	if placeholder != nil && placeholder.SessionRuntime() != nil && placeholder.Session() != nil && filepath.Clean(placeholder.Session().WorkingDir) == filepath.Clean(identity.RootWorkingDir) {
		application = placeholder
	} else if identity.RootWorkingDir != "" && filepath.Clean(originDir) != filepath.Clean(identity.RootWorkingDir) {
		spawner := owner.Spawner()
		if spawner == nil {
			return errors.New("ordinary foreign session restore requires a directory-bound session spawner")
		}
		spawned, err := spawner(ctx, identity.RootWorkingDir)
		if err != nil {
			return err
		}
		application = spawned.App
		if spawned.Ownership == supervisor.RuntimeOwned && spawned.Cleanup != nil {
			cleanup = sync.OnceFunc(spawned.Cleanup)
		}
		if application == nil || application.SessionRuntime() == nil {
			if cleanup != nil {
				cleanup()
			}
			return errors.New("session spawner returned no canonical runtime")
		}
		// The legacy spawner may have exposed its ordinary blank session. This
		// consumer owns only that App observation, not permission to release an
		// arbitrary canonical handle returned by a borrowed spawner.
		application.Close()
	}
	if err := owner.RegisterSessionOwner(identity, compatibilityResources(application, cleanup), false); err != nil {
		if cleanup != nil {
			cleanup()
		}
		// An already admitted winner must be reused by the following host call.
		var conflict *runtime.SessionError
		if errors.As(err, &conflict) && conflict.Kind == runtime.SessionErrorConflict {
			return nil
		}
		return err
	}
	return nil
}

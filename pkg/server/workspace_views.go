package server

import (
	"context"
	"sync"

	"github.com/docker/docker-agent/pkg/runtime"
)

// acquireViewRuntime routes cold IDs by persisted root workspace, never by a
// browser row or the process cwd. The runtime remains the sole authority for
// membership validation and publication.
func (w *workspaceSessionRuntimes) acquireViewRuntime(ctx context.Context, id string) (runtime.SessionRuntime, workspaceKey, func(), error) {
	if rt, key, ok := w.owner(id); ok {
		w.mu.Lock()
		entry := w.runtimes[key]
		if w.closed || entry == nil {
			w.mu.Unlock()
			return nil, workspaceKey{}, nil, runtime.ErrSessionClosed
		}
		entry.refs++
		w.mu.Unlock()
		return rt, key, func() { w.releaseRef(key) }, nil
	}
	if w.store == nil {
		return nil, workspaceKey{}, nil, runtime.UnsupportedSessionOperation(id, "prepare_view")
	}
	selected, err := w.store.GetSession(ctx, id)
	if err != nil {
		return nil, workspaceKey{}, nil, err
	}
	seen := map[string]bool{}
	root := selected
	for depth := 0; ; depth++ {
		if err := ctx.Err(); err != nil {
			return nil, workspaceKey{}, nil, err
		}
		if root == nil || seen[root.ID] || depth >= 32 {
			return nil, workspaceKey{}, nil, &runtime.SessionError{Kind: runtime.SessionErrorInvalid, SessionID: id, Operation: "restore_ancestry"}
		}
		seen[root.ID] = true
		if root.ParentID == "" {
			break
		}
		root, err = w.store.GetSession(ctx, root.ParentID)
		if err != nil {
			return nil, workspaceKey{}, nil, err
		}
	}
	// Prefer an already-live root's generation even when the source refreshed.
	if rt, key, ok := w.owner(root.ID); ok {
		w.mu.Lock()
		entry := w.runtimes[key]
		if w.closed || entry == nil {
			w.mu.Unlock()
			return nil, workspaceKey{}, nil, runtime.ErrSessionClosed
		}
		entry.refs++
		w.mu.Unlock()
		return rt, key, func() { w.releaseRef(key) }, nil
	}
	return w.acquireRuntime(root.WorkingDir)
}

func (w *workspaceSessionRuntimes) ConfirmedSessionViewInfo(ctx context.Context, id string) (runtime.PreparedSessionViewInfo, error) {
	rt, _, release, err := w.acquireViewRuntime(ctx, id)
	if err != nil {
		return runtime.PreparedSessionViewInfo{}, err
	}
	defer release()
	reader, ok := rt.(runtime.SessionViewInfoReader)
	if !ok {
		return runtime.PreparedSessionViewInfo{}, runtime.UnsupportedSessionOperation(id, "prepare_view")
	}
	return reader.ConfirmedSessionViewInfo(ctx, id)
}

func (w *workspaceSessionRuntimes) PrepareSessionView(ctx context.Context, id string) (runtime.PreparedSessionView, error) {
	rt, key, release, err := w.acquireViewRuntime(ctx, id)
	if err != nil {
		return nil, err
	}
	preparer, ok := rt.(runtime.SessionViewPreparer)
	if !ok {
		release()
		return nil, runtime.UnsupportedSessionOperation(id, "prepare_view")
	}
	prepared, err := preparer.PrepareSessionView(ctx, id)
	if err != nil {
		release()
		return nil, err
	}
	p := &workspacePreparedView{PreparedSessionView: prepared, router: w, key: key, release: release}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.stop = context.AfterFunc(ctx, p.Abort)
	return p, nil
}

// This lease owns only a decorator reference, not a session lifecycle. Commit
// gets a separate operation pin so concurrent Abort cannot retire its runtime.
type workspacePreparedView struct {
	runtime.PreparedSessionView

	router  *workspaceSessionRuntimes
	key     workspaceKey
	release func()
	once    sync.Once
	mu      sync.Mutex
	aborted bool
	stop    func() bool
}

func (p *workspacePreparedView) Commit(ctx context.Context) (runtime.CommittedSessionView, error) {
	p.mu.Lock()
	if p.aborted {
		p.mu.Unlock()
		return runtime.CommittedSessionView{}, runtime.ErrSessionClosed
	}
	p.router.mu.Lock()
	entry := p.router.runtimes[p.key]
	if p.router.closed || entry == nil {
		p.router.mu.Unlock()
		p.mu.Unlock()
		return runtime.CommittedSessionView{}, runtime.ErrSessionClosed
	}
	entry.refs++
	p.router.mu.Unlock()
	p.mu.Unlock()
	defer p.router.releaseRef(p.key)
	result, err := p.PreparedSessionView.Commit(ctx)
	if err == nil {
		p.router.remember(result.Info.RootSessionID, p.key)
		p.router.remember(result.Info.SessionID, p.key)
	}
	return result, err
}

func (p *workspacePreparedView) Abort() {
	p.once.Do(func() {
		p.mu.Lock()
		p.aborted = true
		stop := p.stop
		p.mu.Unlock()
		if stop != nil {
			stop()
		}
		p.PreparedSessionView.Abort()
		p.release()
	})
}

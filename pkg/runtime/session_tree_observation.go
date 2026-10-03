package runtime

import (
	"context"
	"errors"
	"sync"

	"github.com/docker/docker-agent/pkg/subagent"
)

func (h *sessionHandle) observeTree(ctx context.Context, options ObserveOptions) (Observation, error) {
	return h.observeTreeWith(ctx, options, func(ctx context.Context, sessionID string, options ObserveOptions) (Observation, error) {
		handle, err := h.runtime.SessionByID(sessionID)
		if err != nil {
			return Observation{}, err
		}
		return handle.Observe(ctx, options)
	})
}

func (h *sessionHandle) observeTreeWith(ctx context.Context, options ObserveOptions, observe func(context.Context, string, ObserveOptions) (Observation, error)) (Observation, error) {
	if options.Since != nil || options.SinceEpoch != "" {
		return Observation{}, &SessionError{Kind: SessionErrorInvalid, SessionID: h.sessionID, Operation: "observe_tree_cursor"}
	}
	if err := ctx.Err(); err != nil {
		return Observation{}, err
	}
	buffer := options.Buffer
	if buffer <= 0 {
		buffer = defaultEventChannelCapacity
	}
	h.runtime.subagents.ensureRoot(h.driver.session(), h.agentName)
	topology, cancelTopology := h.runtime.subagents.tree.Subscribe(buffer)
	initial, ok := <-topology
	if !ok {
		cancelTopology()
		return Observation{}, &SessionError{Kind: SessionErrorStopped, SessionID: h.sessionID, Operation: "observe_tree"}
	}
	root, ok := subtreeForSession(initial, h.sessionID)
	if !ok {
		cancelTopology()
		return Observation{}, &SessionError{Kind: SessionErrorNotFound, SessionID: h.sessionID, Operation: "observe_tree"}
	}

	obsCtx, cancelCtx := context.WithCancel(ctx)
	events := make(chan SessionEvent, buffer)
	sessionsAdded := make(chan SessionSnapshot, buffer)
	errorsCh := make(chan error, 1)
	observations := make(map[string]Observation)
	var snapshots []SessionSnapshot
	var replay []SessionEvent
	var wg sync.WaitGroup
	start := func(sessionID string, collect bool) error {
		if _, exists := observations[sessionID]; exists {
			return nil
		}
		observation, err := observe(obsCtx, sessionID, ObserveOptions{Buffer: buffer})
		if err != nil {
			return err
		}
		observations[sessionID] = observation
		if collect {
			snapshots = append(snapshots, observation.Primary())
		} else {
			select {
			case sessionsAdded <- observation.Primary():
			case <-obsCtx.Done():
				observation.Cancel()
				return nil
			}
		}
		replay = append(replay, observation.Replay...)
		wg.Add(1)
		go func(observedSessionID string) {
			defer wg.Done()
			defer observation.Cancel()
			for {
				select {
				case <-obsCtx.Done():
					return
				case err, open := <-observation.Errors:
					if !open {
						observation.Errors = nil
						continue
					}
					if err != nil {
						select {
						case errorsCh <- err:
						default:
						}
					}
				case envelope, open := <-observation.Events:
					if !open {
						if obsCtx.Err() == nil {
							select {
							case errorsCh <- errors.New("tree descendant observation closed: " + observedSessionID):
							default:
							}
							cancelCtx()
						}
						return
					}
					select {
					case events <- envelope:
					case <-obsCtx.Done():
						return
					}
				}
			}
		}(sessionID)
		return nil
	}
	cleanup := func() {
		cancelCtx()
		cancelTopology()
		for _, observation := range observations {
			observation.Cancel()
		}
		wg.Wait()
	}
	for _, sessionID := range subtreeSessionIDs(root) {
		if err := start(sessionID, true); err != nil {
			cleanup()
			return Observation{}, err
		}
	}
	if len(snapshots) == 0 {
		cancelCtx()
		cancelTopology()
		return Observation{}, errors.New("tree observation has no sessions")
	}

	go func() {
		defer close(events)
		defer close(sessionsAdded)
		defer close(errorsCh)
		defer cancelTopology()
		for {
			select {
			case <-obsCtx.Done():
				for _, observation := range observations {
					observation.Cancel()
				}
				wg.Wait()
				return
			case snapshot, open := <-topology:
				if !open {
					cancelCtx()
					continue
				}
				node, found := subtreeForSession(snapshot, h.sessionID)
				if !found {
					continue
				}
				for _, sessionID := range subtreeSessionIDs(node) {
					if err := start(sessionID, false); err != nil {
						select {
						case errorsCh <- err:
						default:
						}
					}
				}
			}
		}
	}()
	var once sync.Once
	cancel := func() { once.Do(cancelCtx) }
	return Observation{Initial: snapshots, SessionsAdded: sessionsAdded, Replay: replay, Events: events, Errors: errorsCh, Cancel: cancel}, nil
}

func subtreeForSession(snapshot subagent.Snapshot, sessionID string) (subagent.NodeSnapshot, bool) {
	var find func([]subagent.NodeSnapshot) (subagent.NodeSnapshot, bool)
	find = func(nodes []subagent.NodeSnapshot) (subagent.NodeSnapshot, bool) {
		for _, node := range nodes {
			if node.Node.SessionID == sessionID || node.Node.ID == subagent.SessionRootID(sessionID) {
				return node, true
			}
			if found, ok := find(node.Children); ok {
				return found, true
			}
		}
		return subagent.NodeSnapshot{}, false
	}
	return find(snapshot.Nodes)
}

func subtreeSessionIDs(root subagent.NodeSnapshot) []string {
	ids := make([]string, 0)
	var walk func(subagent.NodeSnapshot)
	walk = func(node subagent.NodeSnapshot) {
		if node.Node.SessionID != "" {
			ids = append(ids, node.Node.SessionID)
		} else if id, ok := nodeRootSessionID(node.Node.ID); ok {
			ids = append(ids, id)
		}
		for _, child := range node.Children {
			walk(child)
		}
	}
	walk(root)
	return ids
}

func nodeRootSessionID(id subagent.NodeID) (string, bool) {
	const prefix = "root:"
	s := string(id)
	if len(s) <= len(prefix) || s[:len(prefix)] != prefix {
		return "", false
	}
	return s[len(prefix):], true
}

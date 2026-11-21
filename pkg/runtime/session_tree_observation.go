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
	buffer = min(buffer, maxSessionEventSubscriberBuffer)
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
	var events chan SessionEvent
	if !options.Tree || !options.OrderedTree {
		events = make(chan SessionEvent)
	}
	var sessionsAdded chan SessionSnapshot
	var updates chan TreeUpdate
	if options.OrderedTree {
		updates = make(chan TreeUpdate)
	} else {
		sessionsAdded = make(chan SessionSnapshot)
	}
	errorsCh := make(chan error, 1)
	observations := make(map[string]Observation)
	var interestMu sync.Mutex
	interests := make(map[string]Observation)
	// stopped holds descendants already stopped when observed: they have no live
	// observation, so their baseline entry is recorded once and never re-observed.
	stopped := make(map[string]bool)
	var snapshots []SessionSnapshot
	var replay []SessionEvent
	var wg sync.WaitGroup
	start := func(sessionID string, collect bool) error {
		if _, exists := observations[sessionID]; exists || stopped[sessionID] {
			return nil
		}
		observation, err := observe(obsCtx, sessionID, ObserveOptions{Buffer: buffer, BackgroundOutputInterest: options.BackgroundOutputInterest})
		var primary SessionSnapshot
		live := err == nil
		if live {
			observations[sessionID] = observation
			interestMu.Lock()
			interests[sessionID] = observation
			interestMu.Unlock()
			primary = observation.Primary()
			if sessionID == h.sessionID {
				h.attachLiveSubtree(&primary)
			}
		} else {
			// A descendant already stopped is a settled part of the tree, not a
			// failed baseline. The watch root and every other failure stay terminal.
			var sessionErr *SessionError
			if sessionID == h.sessionID || !errors.As(err, &sessionErr) || sessionErr.Kind != SessionErrorStopped {
				return err
			}
			// A failed lookup keeps the observe error, so existing handling is unchanged.
			snapshot, settled, lookupErr := h.stoppedDescendantSnapshot(obsCtx, sessionID)
			if lookupErr != nil || !settled {
				return err
			}
			primary = snapshot
			stopped[sessionID] = true
		}
		switch {
		case collect:
			snapshots = append(snapshots, primary)
			replay = append(replay, observation.Replay...)
		case updates != nil:
			select {
			case updates <- TreeUpdate{Snapshot: &primary, Replay: observation.Replay}:
			case <-obsCtx.Done():
				cancelObservation(observation)
				return nil
			}
		default:
			select {
			case sessionsAdded <- primary:
			case <-obsCtx.Done():
				cancelObservation(observation)
				return nil
			}
		}
		if !collect && updates == nil {
			for _, seed := range observation.Replay {
				select {
				case events <- seed:
				case <-obsCtx.Done():
					return nil
				}
			}
		}
		if !live {
			return nil
		}
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
					if updates != nil {
						select {
						case updates <- TreeUpdate{Event: &envelope}:
						case <-obsCtx.Done():
							return
						}
						continue
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
		if events != nil {
			defer close(events)
		}
		if sessionsAdded != nil {
			defer close(sessionsAdded)
		}
		if updates != nil {
			defer close(updates)
		}
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
					// A closed topology is always ready; disable it so only obsCtx.Done drives cleanup.
					topology = nil
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
	cancel := func() {
		once.Do(func() {
			cancelCtx()
			interestMu.Lock()
			defer interestMu.Unlock()
			for _, observation := range interests {
				cancelObservation(observation)
			}
		})
	}
	setInterest := func(sessionID string, processIDs []string) error {
		interestMu.Lock()
		defer interestMu.Unlock()
		if obsCtx.Err() != nil {
			return ErrSessionClosed
		}
		observation, ok := interests[sessionID]
		if !ok {
			return &SessionError{Kind: SessionErrorNotFound, SessionID: sessionID, Operation: "background_output_interest"}
		}
		if observation.SetBackgroundOutputInterest == nil {
			return ErrUnsupported
		}
		return observation.SetBackgroundOutputInterest(sessionID, processIDs)
	}
	return Observation{Initial: snapshots, TreeUpdates: updates, SessionsAdded: sessionsAdded, Replay: replay, Events: events, Errors: errorsCh, Cancel: cancel, SetBackgroundOutputInterest: setInterest}, nil
}

// attachLiveSubtree gives a non-root watch root the live subtree of its own node,
// so a baseline carries descendant stops that have not yet reached its projection.
func (h *sessionHandle) attachLiveSubtree(primary *SessionSnapshot) {
	if primary.Session == nil || primary.Session.ParentID == "" {
		return
	}
	node, ok := subtreeForSession(h.runtime.subagents.tree.Snapshot(), h.sessionID)
	if !ok {
		return
	}
	tree := subagent.Snapshot{Version: subagent.SnapshotVersion, Root: node.Node.ID, Nodes: []subagent.NodeSnapshot{node}}
	primary.Session.SetSubagentTree(&tree)
}

// stoppedDescendantSnapshot describes a descendant whose stop is final, from its
// session and status, which stay readable after its live observation is gone.
// It reports settled=false for any session that is not finally stopped.
func (h *sessionHandle) stoppedDescendantSnapshot(ctx context.Context, sessionID string) (snapshot SessionSnapshot, settled bool, err error) {
	driver, ok := h.runtime.sessionDrivers.Lookup(sessionID)
	if !ok || !driver.stoppedForObservation() {
		return SessionSnapshot{}, false, nil
	}
	handle, err := h.runtime.SessionByID(sessionID)
	if err != nil {
		return SessionSnapshot{}, false, err
	}
	sess, err := handle.Snapshot(ctx)
	if err != nil {
		return SessionSnapshot{}, false, err
	}
	status, err := handle.Status(ctx)
	if err != nil {
		return SessionSnapshot{}, false, err
	}
	status.SessionID = sessionID
	snapshot = SessionSnapshot{Session: sess, Status: status}
	snapshot.Epoch, snapshot.Cursor = driver.events.CurrentCursor(sessionID)
	return snapshot, true, nil
}

func cancelObservation(observation Observation) {
	if observation.Cancel != nil {
		observation.Cancel()
	}
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

package runtime

import (
	"context"
	"errors"

	"github.com/docker/docker-agent/pkg/subagent"
)

func (h *sessionHandle) stopRootTree(ctx context.Context) error {
	m, g := h.runtime.subagents, h.runtime.sessionDrivers
	m.mu.Lock()
	g.mu.Lock()
	if g.drivers[h.sessionID] != h.driver {
		g.mu.Unlock()
		m.mu.Unlock()
		return &SessionError{Kind: SessionErrorStale, SessionID: h.sessionID, Operation: "stop_subtree"}
	}
	if g.stoppedTrees == nil {
		g.stoppedTrees = make(map[string]struct{})
	}
	g.stoppedTrees[h.sessionID] = struct{}{}
	var children []subagent.NodeID
	for id, rec := range m.children {
		if rec.parentSession == h.sessionID {
			children = append(children, id)
		}
	}
	g.mu.Unlock()
	m.mu.Unlock()
	h.driver.StopAll()
	var result error
	for _, id := range children {
		_, err := m.stopChildContext(ctx, h.sessionID, id)
		result = errors.Join(result, err)
	}
	select {
	case <-h.driver.Done():
	case <-ctx.Done():
		return errors.Join(result, ctx.Err())
	}
	h.driver.mu.Lock()
	settling, generation, runErr := h.driver.settling(), h.driver.generation, h.driver.completionRunErr
	h.driver.mu.Unlock()
	if settling {
		h.driver.finishRunContext(ctx, generation, runErr)
	}
	h.driver.mu.Lock()
	defer h.driver.mu.Unlock()
	result = errors.Join(result, h.driver.completionErr)
	return result
}

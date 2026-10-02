package runtime

import (
	"fmt"

	"github.com/docker/docker-agent/pkg/subagent"
)

// SubagentPolicy is an optional runtime capability for new autonomous delegation.
// Changing it never stops existing work or disables human browsing or control.
type SubagentPolicy interface {
	UseSubagents() bool
	SetUseSubagents(bool)
}

// SubagentControl is an optional human control capability. StopSubtree permanently
// stops a child and its descendants, retaining their transcripts for browsing.
// It is distinct from canceling the current turn or closing a view.
type SubagentControl interface {
	StopSubtree(subagent.NodeID) error
}

func (r *LocalRuntime) StopSubtree(id subagent.NodeID) error {
	if r.subagents == nil {
		return fmt.Errorf("subagent control is unavailable")
	}
	r.subagents.mu.Lock()
	rec := r.subagents.children[id]
	parent := ""
	if rec != nil {
		parent = rec.parentSession
	}
	r.subagents.mu.Unlock()
	if parent == "" {
		return fmt.Errorf("no subagent with id %q; roots cannot be stopped here", id)
	}
	_, err := r.subagents.stopChild(parent, id)
	return err
}

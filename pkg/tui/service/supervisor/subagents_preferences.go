package supervisor

import (
	"fmt"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/runtime"
)

// SaveUseSubagents serializes persistence and policy application with owner
// admission. Retained owners count even when all their views are closed. The
// callback must not call back into the supervisor; a failed save changes nothing.
func (s *Supervisor) SaveUseSubagents(enabled bool, current app.Services, save func(bool) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	runtimes := make(map[*runtime.LocalRuntime]struct{})
	add := func(services app.Services) error {
		rt, ok := services.(*runtime.LocalRuntime)
		if !ok {
			return fmt.Errorf("Use subagents is unavailable on a non-local runtime; preference not saved")
		}
		runtimes[rt] = struct{}{}
		return nil
	}
	if err := add(current); err != nil {
		return err
	}
	for _, runner := range s.runners {
		if runner.App != nil {
			if err := add(runner.App.Runtime()); err != nil {
				return err
			}
		}
	}
	for _, owner := range s.ownerResources {
		if !owner.removed && (owner.resources.Services != nil || owner.resources.Sessions != nil) {
			if err := add(owner.resources.Services); err != nil {
				return err
			}
		}
	}
	if err := save(enabled); err != nil {
		return fmt.Errorf("failed to save Use subagents: %w", err)
	}
	s.useSubagents = new(enabled)
	for rt := range runtimes {
		rt.SetUseSubagents(enabled)
	}
	return nil
}

// applySubagentsPolicyLocked also covers factories that began before a save but
// publish their resources after it. It never alters execution or topology.
func (s *Supervisor) applySubagentsPolicyLocked(services app.Services) {
	if s.useSubagents != nil {
		if rt, ok := services.(*runtime.LocalRuntime); ok {
			rt.SetUseSubagents(*s.useSubagents)
		}
	}
}

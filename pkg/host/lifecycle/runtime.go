// Package lifecycle coordinates runtime and toolset lifetime ownership.
package lifecycle

import (
	"context"
	"sync"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/team"
)

// OwnRuntime transfers ownership of a loaded team's toolsets to the supervisor.
// Shared or caller-provided teams must retain their separate lifetime owner.
func OwnRuntime(owner runtime.SessionRuntimeSupervisor, tm *team.Team) runtime.SessionRuntimeSupervisor {
	return &supervisor{SessionRuntimeSupervisor: owner, team: tm}
}

type supervisor struct {
	runtime.SessionRuntimeSupervisor

	team    *team.Team
	mu      sync.Mutex
	drained bool
	stopped bool
	err     error
}

func (s *supervisor) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped {
		return s.err
	}
	// A timed-out drain is retryable; tools must remain usable until it finishes.
	if !s.drained {
		if err := s.SessionRuntimeSupervisor.Shutdown(ctx); err != nil {
			return err
		}
		s.drained = true
	}
	s.err = s.team.StopToolSets(ctx)
	s.stopped = s.err == nil
	return s.err
}

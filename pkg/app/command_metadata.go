package app

import (
	"context"
	"errors"
	"sync"

	"github.com/docker/docker-agent/pkg/config/types"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/skills"
	"github.com/docker/docker-agent/pkg/tools"
)

type commandMetadata struct {
	commands  types.Commands
	mu        sync.RWMutex
	handle    runtime.SessionHandle
	prompts   map[string]tools.PromptInfo
	promptErr error
	skills    []skills.Skill
	skillsErr error
	statuses  []tools.ToolsetStatus
}

// RefreshCommandMetadata is explicit I/O for a command worker, never a getter.
// Results from a replaced session cannot populate the new session's completion.
func (a *App) RefreshCommandMetadata(ctx context.Context) error {
	h := a.SessionHandle()
	if h == nil {
		return nil
	}
	var commands types.Commands
	if provider, ok := h.(runtime.SessionAgentInfoProvider); ok {
		info, err := provider.SessionAgentInfo(ctx)
		if err != nil {
			return err
		}
		commands = info.Commands
	}
	var prompts map[string]tools.PromptInfo
	var promptErr error
	if h.Metadata().Capabilities.MCPPrompts {
		if provider, ok := h.(runtime.SessionMCPPrompts); ok {
			prompts, promptErr = provider.MCPPrompts(ctx)
		} else {
			promptErr = runtime.UnsupportedSessionOperation(h.ID(), "mcp_prompts")
		}
	}
	list, skillsErr := h.Skills(ctx)
	var statuses []tools.ToolsetStatus
	var toolErr error
	if provider, ok := h.(runtime.SessionToolInspector); ok && h.Metadata().Capabilities.ToolInspection {
		info, err := provider.InspectTools(ctx)
		toolErr = err
		if err == nil {
			statuses = ToolsetStatuses(info)
		}
	}
	if a.SessionHandle() != h {
		return nil
	}
	a.commandMetadata.mu.Lock()
	defer a.commandMetadata.mu.Unlock()
	a.commandMetadata.handle = h
	a.commandMetadata.commands = commands
	a.commandMetadata.prompts = prompts
	a.commandMetadata.promptErr = promptErr
	a.commandMetadata.skills = list
	a.commandMetadata.skillsErr = skillsErr
	a.commandMetadata.statuses = statuses
	return errors.Join(promptErr, skillsErr, toolErr)
}

func (a *App) CachedMCPPrompts() (map[string]tools.PromptInfo, error) {
	a.commandMetadata.mu.RLock()
	defer a.commandMetadata.mu.RUnlock()
	if a.commandMetadata.handle != a.SessionHandle() {
		return nil, nil
	}
	return a.commandMetadata.prompts, a.commandMetadata.promptErr
}
func (a *App) CommandSkills(ctx context.Context) ([]skills.Skill, error) {
	if a.SessionHandle() == nil {
		return a.CurrentAgentSkillsContext(ctx)
	}
	a.commandMetadata.mu.RLock()
	defer a.commandMetadata.mu.RUnlock()
	if a.commandMetadata.handle != a.SessionHandle() {
		return nil, nil
	}
	return a.commandMetadata.skills, a.commandMetadata.skillsErr
}

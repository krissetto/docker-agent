package builtins_test

import (
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/hooks"
	"github.com/docker/docker-agent/pkg/hooks/builtins"
)

func TestApplyAgentDefaultsDetachesSharedConfig(t *testing.T) {
	cfg := &hooks.Config{SessionStart: []hooks.Hook{{Type: hooks.HookTypeBuiltin, Command: "user", Args: []string{"original"}, Env: map[string]string{"KEY": "original"}}}}
	before := cfg.Clone()
	files := []string{"AGENTS.md"}
	var wg sync.WaitGroup
	for range 2 {
		wg.Go(func() {
			for range 20 {
				effective := builtins.ApplyAgentDefaults(cfg, builtins.AgentDefaults{AddDate: true, AddPromptFiles: files})
				effective.SessionStart[0].Args[0] = "changed"
				effective.SessionStart[0].Env["KEY"] = "changed"
				effective.TurnStart[1].Args[0] = "changed"
			}
		})
	}
	wg.Wait()
	require.Equal(t, before, cfg)
	require.Equal(t, []string{"AGENTS.md"}, files)
}

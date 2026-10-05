package agent

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config/latest"
)

func TestHooksAccessorAndOptionDetachConfiguration(t *testing.T) {
	cfg := &latest.HooksConfig{SessionStart: []latest.HookDefinition{{Type: "builtin", Command: "custom", Args: []string{"original"}, Env: map[string]string{"KEY": "original"}}}}
	a := New("root", "", WithHooks(cfg))
	cfg.SessionStart[0].Args[0] = "caller changed"
	got := a.Hooks()
	require.Equal(t, "original", got.SessionStart[0].Args[0])
	got.SessionStart[0].Args[0] = "accessor changed"
	got.SessionStart[0].Env["KEY"] = "accessor changed"
	require.Equal(t, "original", a.Hooks().SessionStart[0].Args[0])
	require.Equal(t, "original", a.Hooks().SessionStart[0].Env["KEY"])
	require.Nil(t, New("empty", "").Hooks())
}

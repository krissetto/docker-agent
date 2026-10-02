package subagenttool

import (
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/styles"
	"github.com/docker/docker-agent/pkg/tui/types"
)

func TestSpawnAgentIncrementalProjection(t *testing.T) {
	for _, args := range []string{
		`{"agent":"coder","task":"long task with \\\"agent\\\":\\\"fake\\\""}`,
		`{"task":"first task","other":{"agent":"nested"},"agent":"actual"}`,
		`{"agent":"first","agent":"final\u754c"}`,
		`{"agent":"first","other":{"agent":"nested"},"list":[{"agent":"also-nested"}]}`,
		`{"agent":"escaped\"name\\tail","task":"ignored"}`,
	} {
		t.Run(args, func(t *testing.T) {
			var p spawnAgent
			for i := 1; i <= len(args); i++ {
				p.update(args[:i], types.ToolStatusPending)
			}
			projected := p.name
			require.Equal(t, projected, p.update(args, types.ToolStatusRunning), "final authoritative decode matches incremental projection")
			require.NotEmpty(t, projected)
			require.Equal(t, "replacement", p.update(`{"agent":"replacement","task":"new"}`, types.ToolStatusRunning))
			require.Equal(t, "restart", p.update(`{"agent":"restart","task":"`, types.ToolStatusPending))
		})
	}
	var p spawnAgent
	require.Empty(t, p.update(`{"agent":"co`, types.ToolStatusPending), "incomplete names do not need repeated partial-document repair")
	require.Equal(t, "coder", p.update(`{"agent":"coder","task":"`+strings.Repeat("x", 1<<16), types.ToolStatusPending))
	require.Equal(t, "authoritative", p.update(`{"agent":"authoritative"}`, types.ToolStatusRunning))
	require.Empty(t, p.update(`not json`, types.ToolStatusRunning), "replacement cannot retain stale attribution")
	for _, args := range []string{`{"agent":null}`, `{"agent":42}`, `{"agent":{"name":"nested"}}`, `{"agent":"old",BROKEN}`} {
		p.update(args, types.ToolStatusPending)
		require.Empty(t, p.update(args, types.ToolStatusRunning), "args=%s: nonstring or malformed authoritative args must clear the preview", args)
	}
}

func TestSpawnCachedHeaderTracksSpinnerStatusResultAndGeometry(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ar := animation.NewRuntime()
		defer ar.Stop()
		msg := testMessage("spawn_subagent", `{"agent":"coder","task":"partial`, "", types.ToolStatusPending)
		view := NewSpawn(ar, msg, nil, nil)
		view.Init()
		initial := view.View()
		require.Contains(t, ansi.Strip(initial), "Spawning coder")
		msg.ToolCall.Function.Arguments += strings.Repeat("x", 1024)
		require.Equal(t, initial, view.View(), "task-only changes reuse the header")
		initialTick, accepted := ar.Accept(ar.Continue()().(animation.TickMsg))
		require.True(t, accepted)
		_ = initialTick
		time.Sleep(200 * time.Millisecond) //nolint:forbidigo // Advances synctest's fake clock.
		tick, ok := ar.Accept(ar.Continue()().(animation.TickMsg))
		require.True(t, ok)
		view.Update(tick)
		require.NotEqual(t, initial, view.View(), "cached header includes spinner frame")
		msg.ToolStatus = types.ToolStatusRunning
		msg.ToolCall.Function.Arguments = `{"agent":"final-worker","task":"complete"}`
		require.Contains(t, ansi.Strip(view.View()), "final-worker")
		msg.ToolStatus = types.ToolStatusCompleted
		msg.Content = `Spawned subagent "named-worker" (child-id).`
		require.Contains(t, ansi.Strip(view.View()), "Spawned named-worker")
		view.SetSize(12, 0)
		for _, line := range strings.Split(view.View(), "\n") {
			require.LessOrEqual(t, ansi.StringWidth(line), 12)
		}
		msg.ToolStatus = types.ToolStatusError
		msg.Content = "actionable failure"
		require.Contains(t, ansi.Strip(view.View()), "actionable")
	})
}

func TestSpawnCachedHeaderTracksTheme(t *testing.T) {
	original := styles.CurrentTheme()
	t.Cleanup(func() { styles.ApplyTheme(original) })
	msg := testMessage("spawn_subagent", `{"agent":"coder"}`, "", types.ToolStatusRunning)
	view := NewSpawn(animation.NewRuntime(), msg, nil, nil)
	before := view.View()
	theme := *original
	theme.Colors.TextMuted = "#123456"
	styles.ApplyTheme(&theme)
	require.NotEqual(t, before, view.View())
}

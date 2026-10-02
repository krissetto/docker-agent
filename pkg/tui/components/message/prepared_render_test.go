package message

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/styles"
	"github.com/docker/docker-agent/pkg/tui/types"
)

func TestPreparedRenderCapturesPaletteAndRejectsChangedInputs(t *testing.T) {
	original := styles.CurrentTheme()
	t.Cleanup(func() { styles.ApplyTheme(original) })
	content := strings.Repeat("# Heading\n\nA **bold** paragraph.\n\n", 1000)
	v := New(animation.NewRuntime(), types.Agent(types.MessageTypeAssistant, "worker", content), nil)
	v.SetSize(100, 0)
	prepare := PrepareRender(v)
	require.NotNil(t, prepare)
	theme := *original
	theme.Colors.Background = "#ffffff"
	styles.ApplyTheme(&theme)
	result := make(chan *PreparedRender, 1)
	go func() { result <- prepare() }()
	require.False(t, ApplyPreparedRender(v, <-result))
	prepare = PrepareRender(v)
	prepared := prepare()
	require.True(t, ApplyPreparedRender(v, prepared))
	start := time.Now()
	require.Equal(t, prepared.output, v.View())
	t.Log("prepared oversized View", time.Since(start))
	v.SetSize(80, 0)
	require.False(t, v.preparedValid(80))
	v.SetSize(100, 0)
	v.AppendContent("new content")
	require.False(t, v.preparedValid(100))
}

func TestPreparedRenderReplacementAndGroupingParity(t *testing.T) {
	content := strings.Repeat("A **bold** paragraph.\n\n", 800)
	for _, change := range []string{"sender", "type", "grouping", "media", "invalidate"} {
		t.Run(change, func(t *testing.T) {
			previous := types.Agent(types.MessageTypeAssistant, "worker", "previous")
			msg := types.Agent(types.MessageTypeAssistant, "worker", content)
			msg.AssistantMedia = []types.AssistantMedia{{ID: 1, Fallback: "pending media"}}
			v := New(animation.NewRuntime(), msg, previous)
			prepared := PrepareRender(v)()
			require.True(t, ApplyPreparedRender(v, prepared))
			switch change {
			case "sender":
				msg.Sender = "other"
				v.SetMessage(msg)
			case "type":
				msg.Type = types.MessageTypeUser
				v.SetMessage(msg)
			case "grouping":
				previous.Type = types.MessageTypeUser
			case "media":
				msg.AssistantMedia[0] = types.AssistantMedia{ID: 1, Fallback: "resolved media"}
				v.SetMessage(msg)
			case "invalidate":
				v.InvalidateRenderCache()
			}
			require.False(t, v.preparedValid(v.width))
			require.False(t, ApplyPreparedRender(v, prepared), "stale worker completion must not revive an invalid artifact")
			fresh := New(animation.NewRuntime(), msg, previous)
			require.Equal(t, fresh.View(), v.View())
		})
	}
}

func TestPreparedRenderMediaAndCodeBlockParity(t *testing.T) {
	msg := types.Agent(types.MessageTypeAssistant, "worker", strings.Repeat("A paragraph.\n\n", 1400)+"```go\nhello()\n```\n")
	msg.AssistantMedia = []types.AssistantMedia{{ID: 1, Fallback: "resolved media"}}
	v := New(animation.NewRuntime(), msg, nil)
	fresh := New(animation.NewRuntime(), msg, nil)
	require.True(t, ApplyPreparedRender(v, PrepareRender(v)()))
	require.Equal(t, fresh.View(), v.View())
	require.Equal(t, fresh.CodeBlocks(), v.CodeBlocks())
}

func TestFinalizeReleasesPreparedRender(t *testing.T) {
	v := New(animation.NewRuntime(), types.Agent(types.MessageTypeAssistant, "worker", strings.Repeat("paragraph\n\n", 1700)), nil)
	require.True(t, ApplyPreparedRender(v, PrepareRender(v)()))
	require.True(t, v.HasLiveRenderState())
	want := v.View()
	v.Finalize()
	require.Nil(t, v.prepared)
	require.False(t, v.HasLiveRenderState())
	require.Equal(t, want, v.View())
	require.False(t, v.HasLiveRenderState())
}

func TestPreparedWorkerUsesImmutableOwnerSnapshot(t *testing.T) {
	msg := types.Agent(types.MessageTypeAssistant, "worker", strings.Repeat("paragraph **body**\n\n", 1000))
	msg.AssistantMedia = []types.AssistantMedia{{ID: 1, Fallback: "pending media"}}
	v := New(animation.NewRuntime(), msg, nil)
	prepare := PrepareRender(v)
	result := make(chan *PreparedRender, 1)
	go func() { result <- prepare() }()
	// These owner mutations run while the worker renders its frozen snapshot.
	for range 10 {
		msg.Sender = "replacement"
		msg.AssistantMedia[0] = types.AssistantMedia{ID: 1, Fallback: "resolved media"}
		v.SetMessage(msg)
	}
	stale := <-result
	require.Contains(t, stale.output, "pending media")
	require.NotContains(t, stale.output, "resolved media")
	current := PrepareRender(v)()
	require.True(t, ApplyPreparedRender(v, current))
	require.False(t, ApplyPreparedRender(v, stale))
	require.Same(t, current, v.prepared, "an old completion must not evict a newer artifact")
}

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

package message

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/types"
)

func TestResumeAnimationDoesNotRetryMarkdownImages(t *testing.T) {
	ar := animation.NewRuntime()
	t.Cleanup(ar.Stop)
	msg := types.Agent(types.MessageTypeAssistant, "root", "![image](missing-presentation-image.png)")
	m := New(ar, msg, nil)
	// No image command is executed. Empty loading state models a failed prior
	// load, which Init would retry but visibility must leave alone.
	m.loadingImages = map[string]bool{}
	for range 3 {
		m.StopAnimation()
		require.Nil(t, m.ResumeAnimation())
		require.Empty(t, m.loadingImages)
		require.False(t, ar.HasActive())
	}
	pending := New(ar, types.Spinner(), nil)
	require.NotNil(t, pending.ResumeAnimation())
	require.Equal(t, int32(1), ar.ActiveCount())
	require.Nil(t, pending.ResumeAnimation())
	pending.StopAnimation()
	require.False(t, ar.HasActive())
}

package chat

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPageHeightResizeReusesTranscript(t *testing.T) {
	p := newTestChatPage(t)
	t.Cleanup(p.messages.StopAnimations)
	p.SetSize(160, 40)
	for i := range 200 {
		p.messages.AddUserMessage(fmt.Sprintf("history %d", i))
	}
	_ = p.View()
	rebuilds, misses, renders := p.ResizeCacheStats()
	for _, height := range []int{39, 36, 40, 42} {
		p.SetSize(160, height)
		_ = p.View()
		r, miss, render := p.ResizeCacheStats()
		require.Equal(t, rebuilds, r)
		require.Equal(t, misses, miss)
		require.Equal(t, renders, render)
	}
}

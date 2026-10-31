package rulebased

import (
	"github.com/docker/docker-agent/pkg/model/provider/contracts"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
)

func TestServingModelConcurrentPerStream(t *testing.T) {
	t.Parallel()

	cfg := &latest.ModelConfig{
		Provider: "openai",
		Model:    "gpt-4o",
		Routing: []latest.RoutingRule{
			{
				Model:    "anthropic/claude-3-haiku",
				Examples: []string{"hello", "hi there"},
			},
		},
	}

	client, err := NewClient(t.Context(), cfg, nil, nil, mockProviderFactory)
	require.NoError(t, err)
	defer client.Close()

	var wg sync.WaitGroup
	for range 100 {
		wg.Add(2)
		go func() {
			defer wg.Done()
			messages := []chat.Message{{Role: chat.MessageRoleUser, Content: "hello"}}
			_, _ = client.CreateChatCompletionStream(t.Context(), messages, nil)
		}()
		go func() {
			defer wg.Done()
			messages := []chat.Message{{Role: chat.MessageRoleUser, Content: "hi there"}}
			stream, err := client.CreateChatCompletionStream(t.Context(), messages, nil)
			if err != nil || stream == nil {
				return
			}
			// The serving model is immutable per call and read from that call's stream.
			if served, ok := stream.(contracts.ServedStream); ok {
				_, _ = served.ServedBy()
			}
		}()
	}
	wg.Wait()
}

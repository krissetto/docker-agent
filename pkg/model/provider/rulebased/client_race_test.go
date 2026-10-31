package rulebased

import (
	"context"
	"io"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/environment"
	"github.com/docker/docker-agent/pkg/model/provider/contracts"
	"github.com/docker/docker-agent/pkg/model/provider/options"
	"github.com/docker/docker-agent/pkg/modelsdev"
	"github.com/docker/docker-agent/pkg/tools"
)

// servingStubProvider returns a real stream, so the tests can read its serving metadata.
type servingStubProvider struct{ Provider }

func (servingStubProvider) CreateChatCompletionStream(context.Context, []chat.Message, []tools.Tool) (chat.MessageStream, error) {
	return servingStubStream{}, nil
}

type servingStubStream struct{}

func (servingStubStream) Recv() (chat.MessageStreamResponse, error) {
	return chat.MessageStreamResponse{}, io.EOF
}

func (servingStubStream) Close() {}

func servingStubFactory(ctx context.Context, modelSpec string, models map[string]latest.ModelConfig, env environment.Provider, opts ...options.Opt) (Provider, error) {
	p, err := mockProviderFactory(ctx, modelSpec, models, env, opts...)
	return servingStubProvider{Provider: p}, err
}

// Concurrent streams must each report their own route: there is no shared
// last-selected state for them to read.
func TestRoutedStreamServingModel_Concurrent(t *testing.T) {
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

	client, err := NewClient(t.Context(), cfg, nil, nil, servingStubFactory)
	require.NoError(t, err)
	defer client.Close()

	var wg sync.WaitGroup
	for range 100 {
		wg.Add(2)
		go func() {
			defer wg.Done()
			messages := []chat.Message{{Role: chat.MessageRoleUser, Content: "hello"}}
			stream, err := client.CreateChatCompletionStream(t.Context(), messages, nil)
			if assert.NoError(t, err) && assert.NotNil(t, stream) {
				reporter, ok := stream.(contracts.ServingStream)
				if assert.True(t, ok) {
					assert.Equal(t, modelsdev.NewID("anthropic", "claude-3-haiku"), reporter.ServingModel().ID)
				}
				stream.Close()
			}
		}()
		go func() {
			defer wg.Done()
			messages := []chat.Message{{Role: chat.MessageRoleUser, Content: "unrelated"}}
			stream, err := client.CreateChatCompletionStream(t.Context(), messages, nil)
			if assert.NoError(t, err) && assert.NotNil(t, stream) {
				reporter, ok := stream.(contracts.ServingStream)
				if assert.True(t, ok) {
					assert.Equal(t, client.ID(), reporter.ServingModel().ID)
				}
				stream.Close()
			}
		}()
	}
	wg.Wait()
}

package runtime

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/model/provider"
	"github.com/docker/docker-agent/pkg/model/provider/base"
	"github.com/docker/docker-agent/pkg/model/provider/openai"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/teamloader"
	"github.com/docker/docker-agent/pkg/tools"
)

type imageOnlyProvider struct{ imageOutputProvider }

func (p *imageOnlyProvider) BaseConfig() base.Config {
	config := p.imageOutputProvider.BaseConfig()
	config.ModelConfig.ProviderOpts = map[string]any{"api_type": "openai_images", "context_size": 128000}
	return config
}

func TestTitleGeneratorSkipsImageOnlyModel(t *testing.T) {
	t.Parallel()
	model := &imageOnlyProvider{imageOutputProvider: imageOutputProvider{id: "openai/gpt-image-1"}}
	rt := newTitleTestRuntime(t, agent.New("root", "", agent.WithModel(model)))
	assert.Nil(t, rt.TitleGenerator(t.Context()))
	assert.Zero(t, model.calls.Load())
}

func TestCompactionSkipsImageOnlyModel(t *testing.T) {
	t.Parallel()
	model := &imageOnlyProvider{imageOutputProvider: imageOutputProvider{id: "openai/gpt-image-1"}}
	root := agent.New("root", "", agent.WithModel(model))
	rt := newTitleTestRuntime(t, root)
	sess := session.New(session.WithUserMessage("draw a cat"))
	sink := &collectingSink{}
	rt.doCompact(t.Context(), sess, root, "", compactionReasonManual, sink)
	assert.Zero(t, model.calls.Load())
	assert.Len(t, sess.MessagesSnapshot(), 1)
	require.Len(t, sink.warnings(), 1)
	assert.Contains(t, sink.warnings()[0].Message, "text-capable compaction_model")
}

func TestImageOnlyMainStreamPersistsMedia(t *testing.T) {
	model := &imageOnlyProvider{imageOutputProvider: imageOutputProvider{
		id:     "openai/gpt-image-1",
		stream: newStreamBuilder().AddMedia([]byte("image bytes"), "image/png", "cat.png").AddStopWithUsage(10, 5).Build(),
	}}
	root := agent.New("root", "", agent.WithModel(model))
	rt := newTitleTestRuntime(t, root)
	store := session.NewInMemorySessionStore()
	rt.sessionStore = store
	sess := session.New(session.WithUserMessage("draw a cat"))
	sess.WorkingDir = t.TempDir()
	require.NoError(t, store.AddSession(t.Context(), sess))
	for _, event := range runAndCollect(t, rt, sess) {
		if failure, ok := event.(*ErrorEvent); ok {
			t.Fatalf("main stream failed: %s", failure.Error)
		}
	}
	assert.Equal(t, int64(1), model.calls.Load())
	messages := sess.MessagesSnapshot()
	require.Len(t, messages, 2)
	require.Len(t, messages[1].Message.Message.MultiContent, 1)
	doc := messages[1].Message.Message.MultiContent[0].Document
	require.NotNil(t, doc)
	assert.Equal(t, "cat.png", doc.Source.ArtifactPath)
	blobs, ok := store.(session.GeneratedMediaBlobStore)
	require.True(t, ok)
	data, err := blobs.LookupGeneratedBlob(context.Background(), sess.ID, doc.Source.ArtifactPath)
	require.NoError(t, err)
	assert.Equal(t, []byte("image bytes"), data)
}

// Pure-image child requests must not inherit automatic list/send tools.
type imageOnlyChildProvider struct {
	imageOnlyProvider
	offered []tools.Tool
}

func (p *imageOnlyChildProvider) CreateChatCompletionStream(ctx context.Context, messages []chat.Message, ts []tools.Tool) (chat.MessageStream, error) {
	p.offered = ts
	return p.imageOnlyProvider.CreateChatCompletionStream(ctx, messages, ts)
}
func TestImageOnlyAsyncChildSkipsAutomaticTools(t *testing.T) {
	model := &imageOnlyChildProvider{imageOnlyProvider: imageOnlyProvider{imageOutputProvider: imageOutputProvider{
		id: "openai/gpt-image-1", stream: newStreamBuilder().AddMedia([]byte("image"), "image/png", "child.png").Build(),
	}}}
	root := agent.New("root", "", agent.WithModel(model))
	rt := newTitleTestRuntime(t, root)
	sess := session.New(session.WithUserMessage("draw a cat"), session.WithAsyncSubagent(true))
	sess.WorkingDir = t.TempDir()
	for _, event := range runAndCollect(t, rt, sess) {
		if failure, ok := event.(*ErrorEvent); ok {
			t.Fatalf("child failed: %s", failure.Error)
		}
	}
	assert.Empty(t, model.offered)
	assert.Equal(t, int64(1), model.calls.Load())
	explicit := []tools.Tool{{Name: "explicit_user_tool"}}
	assert.Equal(t, explicit, rt.prepareTurnTools(t.Context(), sess, root, explicit), "explicit tools remain visible to provider validation")
}

func TestNamedImageProviderSkipsAuxiliaryTextTasks(t *testing.T) {
	t.Parallel()
	source := config.NewBytesSource("image.yaml", []byte(`
providers:
  local_images:
    api_type: openai_images
    base_url: http://127.0.0.1:1/v1
models:
  image:
    provider: local_images
    model: flux2-klein-4b
    output_capabilities:
      image: true
agents:
  root:
    model: image
    toolsets: []
`))
	registry := provider.NewRegistry(map[string]provider.Factory{"openai": provider.Adapt(openai.NewClient)})
	tm, err := teamloader.Load(t.Context(), source, &config.RuntimeConfig{}, teamloader.WithProviderRegistry(registry))
	require.NoError(t, err)
	root, err := tm.Agent("root")
	require.NoError(t, err)
	cfg := root.Model(t.Context()).BaseConfig().ModelConfig
	assert.True(t, cfg.IsImageOnly(), "named provider selection must survive provider construction")
	rt := newTitleTestRuntime(t, root)
	assert.Nil(t, rt.TitleGenerator(t.Context()))
	sink := &collectingSink{}
	rt.doCompact(t.Context(), session.New(session.WithUserMessage("draw")), root, "", compactionReasonManual, sink)
	require.Len(t, sink.warnings(), 1)
	assert.Contains(t, sink.warnings()[0].Message, "text-capable compaction_model")
}

package server

import (
	"bytes"
	"context"
	"image"
	"image/png"
	"io"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/model/provider/base"
	"github.com/docker/docker-agent/pkg/modelsdev"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/tools"
)

type generatedHTTPProvider struct{ data []byte }

func (generatedHTTPProvider) ID() modelsdev.ID        { return modelsdev.ParseIDOrZero("test/media") }
func (generatedHTTPProvider) BaseConfig() base.Config { return base.Config{} }
func (p generatedHTTPProvider) CreateChatCompletionStream(context.Context, []chat.Message, []tools.Tool) (chat.MessageStream, error) {
	return &generatedHTTPStream{data: p.data}, nil
}

type generatedHTTPStream struct {
	data  []byte
	index int
}

func (s *generatedHTTPStream) Recv() (chat.MessageStreamResponse, error) {
	s.index++
	if s.index == 1 {
		return chat.MessageStreamResponse{Choices: []chat.MessageStreamChoice{{Delta: chat.MessageDelta{Content: "generated", Media: []chat.MediaDelta{{Data: s.data, MimeType: "image/png", Name: "cat.png"}}}}}}, nil
	}
	if s.index == 2 {
		return chat.MessageStreamResponse{Choices: []chat.MessageStreamChoice{{FinishReason: chat.FinishReasonStop}}}, nil
	}
	return chat.MessageStreamResponse{}, io.EOF
}
func (*generatedHTTPStream) Close() {}

func TestGeneratedMediaHTTPLocalRemoteLiveReplayParity(t *testing.T) {
	var pngData bytes.Buffer
	require.NoError(t, png.Encode(&pngData, image.NewRGBA(image.Rect(0, 0, 2, 1))))
	store := session.NewInMemorySessionStore()
	rt, err := runtime.NewLocalRuntime(t.Context(), team.New(team.WithAgents(agent.New("root", "prompt", agent.WithModel(generatedHTTPProvider{data: pngData.Bytes()})))), runtime.WithSessionStore(store))
	require.NoError(t, err)
	supervisor := runtime.NewSessionRuntimeSupervisor(rt)
	t.Cleanup(func() { require.NoError(t, supervisor.Shutdown(context.WithoutCancel(t.Context()))) })
	sess := session.New(session.WithID("media-owner"))
	sess.WorkingDir = t.TempDir()
	local, err := rt.CreateSession(t.Context(), sess, runtime.SessionBinding{AgentName: "root"})
	require.NoError(t, err)
	manager := NewSessionManager(t.Context(), config.Sources{}, store, 0, &config.RuntimeConfig{}, WithSessionRuntime(supervisor.Runtime()))
	handler := NewWithManager(manager, "")
	srv := httptest.NewServer(handler.e)
	defer srv.Close()
	client, err := runtime.NewClient(srv.URL, runtime.WithHTTPClient(srv.Client()))
	require.NoError(t, err)
	transport, err := runtime.NewSessionTransport(client)
	require.NoError(t, err)
	remote, err := transport.SessionByID(sess.ID)
	require.NoError(t, err)
	observation, err := remote.Observe(t.Context(), runtime.ObserveOptions{})
	require.NoError(t, err)
	defer observation.Cancel()
	submission, err := remote.Submit(t.Context(), runtime.TurnInput{Content: "draw"})
	require.NoError(t, err)
	require.NoError(t, remote.AwaitTurn(t.Context(), submission.TurnID))
	var added *runtime.MessageAddedEvent
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	for added == nil {
		select {
		case event := <-observation.Events:
			if message, ok := event.Event.(*runtime.MessageAddedEvent); ok && len(message.GeneratedMedia) > 0 {
				added = message
			}
		case <-deadline.C:
			t.Fatal("no media event")
		}
	}
	require.Nil(t, added.Message)
	require.Len(t, added.GeneratedMedia, 1)
	source := added.GeneratedMedia[0].Document.Source
	ref := runtime.GeneratedFileRef{OwnerSessionID: source.ArtifactOwnerSessionID, Root: source.ArtifactRoot, Path: source.ArtifactPath}
	localData, err := local.(runtime.GeneratedFileResolver).ResolveGeneratedFile(t.Context(), ref)
	require.NoError(t, err)
	remoteData, err := remote.(runtime.GeneratedFileResolver).ResolveGeneratedFile(t.Context(), ref)
	require.NoError(t, err)
	assert.Equal(t, pngData.Bytes(), localData.Data)
	assert.Equal(t, localData.Data, remoteData.Data)
	assert.Equal(t, ref.Path, remoteData.Path)
	replay, err := remote.Observe(t.Context(), runtime.ObserveOptions{})
	require.NoError(t, err)
	defer replay.Cancel()
	require.NotEmpty(t, replay.Primary().Session.Messages)
	branch := session.New(session.WithID("media-branch"))
	branch.WorkingDir = t.TempDir()
	branch.AddMessage(session.NewAgentMessage("root", &chat.Message{Role: chat.MessageRoleAssistant, MultiContent: added.GeneratedMedia}))
	_, err = rt.CreateSession(t.Context(), branch, runtime.SessionBinding{AgentName: "root"})
	require.NoError(t, err)
	branchRemote, err := transport.SessionByID(branch.ID)
	require.NoError(t, err)
	inherited, err := branchRemote.(runtime.GeneratedFileResolver).ResolveGeneratedFile(t.Context(), ref)
	require.NoError(t, err)
	assert.Equal(t, localData.Data, inherited.Data)
	foreign := session.New(session.WithID("foreign"))
	foreign.WorkingDir = t.TempDir()
	_, err = rt.CreateSession(t.Context(), foreign, runtime.SessionBinding{AgentName: "root"})
	require.NoError(t, err)
	foreignRemote, err := transport.SessionByID(foreign.ID)
	require.NoError(t, err)
	_, err = foreignRemote.(runtime.GeneratedFileResolver).ResolveGeneratedFile(t.Context(), ref)
	require.Error(t, err)
	for _, path := range []string{"../cat.png", "/etc/passwd", "images/../../.env", "cat.png\\secret"} {
		invalid := ref
		invalid.Path = path
		_, err = remote.(runtime.GeneratedFileResolver).ResolveGeneratedFile(t.Context(), invalid)
		require.Error(t, err)
	}
}

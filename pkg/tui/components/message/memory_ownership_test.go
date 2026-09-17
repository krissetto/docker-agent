package message

import (
	"image/color"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/types"
)

func TestStaticHistoryDoesNotAllocateSpinnerAndPendingTransitionsRelease(t *testing.T) {
	ar := animation.NewRuntime()
	t.Cleanup(ar.Stop)
	for _, msg := range []*types.Message{
		types.User("historical user"),
		types.Agent(types.MessageTypeAssistant, "root", "historical assistant"),
		types.Error("historical error"),
	} {
		m := New(ar, msg, nil)
		require.Nil(t, m.spinner)
		_ = m.Init()
		_ = m.View()
		m.StopAnimation()
		require.Nil(t, m.ResumeAnimation())
		require.Nil(t, m.spinner, "static history must not allocate frames on render/visibility")
	}
	m := New(ar, types.Spinner(), nil)
	require.NotNil(t, m.spinner)
	pending := m.spinner
	_ = m.Init()
	require.EqualValues(t, 1, ar.ActiveCount())
	m.StopAnimation()
	_ = m.ResumeAnimation()
	require.Same(t, pending, m.spinner, "visibility preserves phase and playful label")
	_ = m.SetMessage(types.Agent(types.MessageTypeAssistant, "root", "committed"))
	require.Nil(t, m.spinner)
	require.Zero(t, ar.ActiveCount(), "type transition releases the old spinner lease")
	_ = m.SetMessage(types.Loading("pending again"))
	require.NotNil(t, m.spinner)
	_ = m.Init()
	require.EqualValues(t, 1, ar.ActiveCount())
	_ = m.SetMessage(types.Agent(types.MessageTypeAssistant, "root", ""))
	require.NotNil(t, m.spinner, "empty assistant still has its fallback spinner")
	_ = m.AppendContent("first token")
	require.Nil(t, m.spinner)
	require.Zero(t, ar.ActiveCount())
	require.Contains(t, m.View(), "first token")
}

func TestCommittedContentBuilderIsLazyAndResumesAfterFinalize(t *testing.T) {
	m := New(animation.NewRuntime(), types.Agent(types.MessageTypeAssistant, "root", "old"), nil)
	content := strings.Repeat("canonical λ界\n", 4096)
	msg := types.Agent(types.MessageTypeAssistant, "root", content)
	_ = m.SetMessage(msg)
	require.Zero(t, m.contentBuf.Cap(), "committed text must not acquire a second mutable copy")
	_ = m.AppendContent(" first")
	require.Equal(t, content+" first", msg.Content)
	m.Finalize()
	require.Zero(t, m.contentBuf.Cap())
	require.Equal(t, content+" first", msg.Content, "reset cannot release canonical text")
	_ = m.AppendContent(" second")
	require.Equal(t, content+" first second", msg.Content)
	require.False(t, m.finalized, "actual streaming resumes its renderer")
	_ = m.SetMessage(types.Agent(types.MessageTypeAssistant, "root", "replacement"))
	require.Zero(t, m.contentBuf.Cap())
	_ = m.AppendContent(" tail")
	require.Equal(t, "replacement tail", m.message.Content)
}

func TestContentBuilderFinalizePreservesPendingAndLoadedMedia(t *testing.T) {
	uri := testImageURI(t, color.RGBA{G: 255, A: 255})
	m := New(animation.NewRuntime(), types.Agent(types.MessageTypeAssistant, "root", ""), nil)
	_ = m.SetMessage(types.Agent(types.MessageTypeAssistant, "root", "prefix ![image"))
	m.Finalize()
	cmd := m.AppendContent("](" + uri + ")")
	require.NotNil(t, cmd, "resume discovers an opener predating Finalize")
	_, _ = m.Update(cmd()) // synthetic in-memory image only
	require.Contains(t, m.markdownImages, uri)
	before := m.message.Content
	m.Finalize()
	require.Contains(t, m.markdownImages, uri, "decoded media is not a disposable render cache")
	require.Nil(t, m.AppendContent(" suffix"), "loaded media must not be fetched again")
	require.Equal(t, before+" suffix", m.message.Content)
	require.Contains(t, m.markdownImages, uri)
}

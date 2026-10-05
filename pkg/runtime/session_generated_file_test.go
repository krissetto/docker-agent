package runtime

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/subagent"
)

func TestRemoteGeneratedFileBoundedAuthenticatedAndCancelled(t *testing.T) {
	ref := workspaceRef("owner", "images/cat.png")
	for _, test := range []struct {
		name     string
		status   int
		size     int
		redirect bool
	}{
		{name: "success", status: 200, size: 3},
		{name: "oversize", status: 200, size: MaxGeneratedFileBytes + 1},
		{name: "foreign", status: 404},
		{name: "redirect", status: 307, redirect: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, "/api/v2/sessions/viewer/generated-media", r.URL.Path)
				assert.Equal(t, "Bearer owned-token", r.Header.Get("Authorization"))
				var got GeneratedFileRef
				if !assert.NoError(t, json.NewDecoder(r.Body).Decode(&got)) {
					return
				}
				assert.Equal(t, ref, got)
				if test.redirect {
					w.Header().Set("Location", "/unscoped")
				}
				w.Header().Set("Content-Type", "application/octet-stream")
				w.WriteHeader(test.status)
				if test.size > 0 {
					_, _ = w.Write([]byte(strings.Repeat("x", test.size)))
				}
			}))
			defer server.Close()
			client, err := NewClient(server.URL, WithHTTPClient(server.Client()), WithAuthToken("owned-token"))
			require.NoError(t, err)
			transport, err := NewSessionTransport(client)
			require.NoError(t, err)
			handle, err := transport.SessionByID("viewer")
			require.NoError(t, err)
			result, err := handle.(GeneratedFileResolver).ResolveGeneratedFile(t.Context(), ref)
			if test.name == "success" {
				require.NoError(t, err)
				assert.Equal(t, []byte("xxx"), result.Data)
				assert.Equal(t, ref.Path, result.Path)
			} else {
				require.Error(t, err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			_, err = handle.(GeneratedFileResolver).ResolveGeneratedFile(ctx, ref)
			require.Error(t, err)
		})
	}
}

func TestGeneratedMediaBoundaryPortableRefs(t *testing.T) {
	part := chat.MessagePart{Type: chat.MessagePartTypeDocument, Document: &chat.Document{Name: "cat.png", MimeType: "image/png", Source: chat.DocumentSource{ArtifactOwnerSessionID: "s", ArtifactRoot: chat.ArtifactRootWorkspace, ArtifactPath: "cat.png", InlineData: []byte("private binary")}}}
	message := session.NewAgentMessage("root", &chat.Message{Role: chat.MessageRoleAssistant, Content: "private body", MultiContent: []chat.MessagePart{part}})
	raw, err := json.Marshal(MessageAddedAt("s", message, "root", 2))
	require.NoError(t, err)
	assert.NotContains(t, string(raw), "private body")
	assert.NotContains(t, string(raw), "inline_data")
	client, err := NewClient("http://localhost")
	require.NoError(t, err)
	event, err := client.decodeSessionEvent(raw)
	require.NoError(t, err)
	added := event.(*MessageAddedEvent)
	require.Nil(t, added.Message)
	require.Len(t, added.GeneratedMedia, 1)
	assert.Equal(t, "cat.png", added.GeneratedMedia[0].Document.Source.ArtifactPath)
	assert.True(t, sessionContainsGeneratedFile(&session.Session{Messages: []session.Item{{Message: message}}}, workspaceRef("s", "cat.png")))
	assert.False(t, sessionContainsGeneratedFile(&session.Session{Messages: []session.Item{{Message: message}}}, workspaceRef("foreign", "cat.png")))
}

func TestGeneratedFileOversizeWorkspaceAndBlob(t *testing.T) {
	sess, root := workspaceSession(t, "oversize")
	rt, store := resolverTestRuntime(t, sess)
	recordWorkspaceFile(t, store, sess.ID, root, "large.png", []byte("x"))
	file, err := os.OpenFile(filepath.Join(root, "large.png"), os.O_WRONLY, 0)
	require.NoError(t, err)
	require.NoError(t, file.Truncate(MaxGeneratedFileBytes+1))
	require.NoError(t, file.Close())
	_, err = rt.ResolveGeneratedFile(t.Context(), workspaceRef(sess.ID, "large.png"))
	require.ErrorIs(t, err, ErrGeneratedFileUnavailable)
	require.NoError(t, store.(session.GeneratedMediaBlobStore).AddGeneratedBlob(t.Context(), sess.ID, "large.png", make([]byte, MaxGeneratedFileBytes+1)))
	_, err = rt.ResolveGeneratedFile(t.Context(), workspaceRef(sess.ID, "large.png"))
	require.ErrorIs(t, err, ErrGeneratedFileUnavailable)
}

func TestGeneratedFileCanonicalDescendantScope(t *testing.T) {
	m := newTestSubagentManager(t)
	root := session.New(session.WithID("viewer"), session.WithAgentName("root"))
	child := session.New(session.WithID("visible-child"), session.WithAgentName("worker"))
	foreign := session.New(session.WithID("unrelated-child"), session.WithAgentName("worker"))
	store := session.NewInMemorySessionStore()
	m.r.sessionStore = store
	for _, sess := range []*session.Session{root, child, foreign} {
		sess.WorkingDir = t.TempDir()
		if sess != root {
			sess.AddMessage(session.NewAgentMessage("worker", &chat.Message{Role: chat.MessageRoleAssistant, MultiContent: []chat.MessagePart{{Type: chat.MessagePartTypeDocument, Document: &chat.Document{Name: "cat.png", MimeType: "image/png", Source: chat.DocumentSource{ArtifactOwnerSessionID: sess.ID, ArtifactRoot: chat.ArtifactRootWorkspace, ArtifactPath: "cat.png"}}}}}))
		}
		require.NoError(t, store.AddSession(t.Context(), sess))
		m.r.sessionDrivers.Get(sess)
	}
	m.ensureRoot(root, "root")
	require.NoError(t, m.tree.Add(subagent.Node{ID: "visible", Agent: "worker", Parent: subagent.SessionRootID(root.ID), SessionID: child.ID, State: subagent.NodeIdle}))
	m.ensureRoot(foreign, "worker")
	recordWorkspaceFile(t, store, child.ID, child.WorkingDir, "cat.png", []byte("child-media"))
	recordWorkspaceFile(t, store, foreign.ID, foreign.WorkingDir, "cat.png", []byte("foreign-media"))
	h := &sessionHandle{runtime: m.r, driver: m.r.sessionDrivers.Get(root), sessionID: root.ID, agentName: "root"}
	resolved, err := h.ResolveGeneratedFile(t.Context(), workspaceRef(child.ID, "cat.png"))
	require.NoError(t, err)
	assert.Equal(t, []byte("child-media"), resolved.Data)
	_, err = h.ResolveGeneratedFile(t.Context(), workspaceRef(foreign.ID, "cat.png"))
	require.ErrorIs(t, err, ErrGeneratedFileUnavailable)
	require.NoError(t, m.tree.Remove("visible"))
	_, err = h.ResolveGeneratedFile(t.Context(), workspaceRef(child.ID, "cat.png"))
	require.ErrorIs(t, err, ErrGeneratedFileUnavailable)
}

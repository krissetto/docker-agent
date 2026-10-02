package root

import (
	"bytes"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/paths"
	"github.com/docker/docker-agent/pkg/session/sqlitestore"
)

func TestSessionsListPromptAndResume(t *testing.T) {
	home := isolateSessionsList(t)
	paths.SetRoot(home)
	t.Chdir(home)
	t.Setenv("TELEMETRY_ENABLED", "false")
	t.Setenv("OPENAI_API_KEY", "test-only")
	t.Setenv("DOCKER_AGENT_HIDE_TELEMETRY_BANNER", "1")
	logger := slog.Default()
	t.Cleanup(func() { slog.SetDefault(logger) })

	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"id\":\"test\",\"object\":\"chat.completion.chunk\",\"model\":\"fixture\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"Offline reply\"}}]}\n\n"+
			"data: {\"id\":\"test\",\"object\":\"chat.completion.chunk\",\"model\":\"fixture\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n"+
			"data: [DONE]\n\n")
	}))
	defer server.Close()
	agentFile := filepath.Join(home, "agent.yaml")
	require.NoError(t, os.WriteFile(agentFile, fmt.Appendf(nil, `version: "9"
providers:
  fixture:
    api_type: openai_chatcompletions
    base_url: %s/v1
models:
  fixture:
    provider: fixture
    model: fixture
    max_tokens: 64
agents:
  root:
    model: fixture
    instruction: Reply concisely.
`, server.URL), 0o600))
	dbPath := filepath.Join(paths.GetDataDir(), "session.db")
	run := func(args ...string) string {
		t.Helper()
		var stdout, stderr bytes.Buffer
		cmd := NewRootCmd()
		cmd.SetIn(strings.NewReader(""))
		cmd.SetOut(&stdout)
		cmd.SetErr(&stderr)
		cmd.SetArgs(args)
		require.NoError(t, cmd.ExecuteContext(t.Context()), "stderr: %s", stderr.String())
		return stdout.String()
	}
	run("run", "--exec", "--session-db", dbPath, agentFile, "--", "first isolated prompt")
	require.Positive(t, calls.Load())
	beforeList := calls.Load()
	listed := run("sessions", "list", "--quiet")
	assert.Equal(t, beforeList, calls.Load(), "listing must not invoke the provider")
	ids := strings.Fields(listed)
	require.Len(t, ids, 1)
	run("run", "--exec", "--session-db", dbPath, "--session", ids[0], agentFile, "--", "second isolated prompt")
	assert.Greater(t, calls.Load(), beforeList)
	assert.Equal(t, listed, run("sessions", "list", "--quiet"), "resume must not create another root")

	store, err := sqlitestore.New(t.Context(), dbPath)
	require.NoError(t, err)
	defer store.Close()
	resumed, err := store.GetSession(t.Context(), ids[0])
	require.NoError(t, err)
	var prompts []string
	for _, item := range resumed.Messages {
		if item.Message != nil && item.Message.Message.Role == chat.MessageRoleUser {
			prompts = append(prompts, item.Message.Message.Content)
		}
	}
	assert.Equal(t, []string{"first isolated prompt", "second isolated prompt"}, prompts)
}

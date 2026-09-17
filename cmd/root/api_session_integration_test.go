package root

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type listeningWriter struct {
	mu      sync.Mutex
	buffer  bytes.Buffer
	address chan string
}

func newListeningWriter() *listeningWriter {
	return &listeningWriter{address: make(chan string, 1)}
}

func (w *listeningWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	n, err := w.buffer.Write(p)
	if match := regexp.MustCompile(`Listening on (127\.0\.0\.1:\d+)`).FindStringSubmatch(w.buffer.String()); len(match) == 2 {
		select {
		case w.address <- match[1]:
		default:
		}
	}
	return n, err
}

func (w *listeningWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buffer.String()
}

func TestServeAPICommandHeadlessSessionBootstrapAndShutdown(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "test-only")
	t.Setenv("CAGENT_HIDE_TELEMETRY_BANNER", "1")
	t.Setenv("DOCKER_AGENT_HIDE_TELEMETRY_BANNER", "1")

	dir := t.TempDir()
	agentFile := filepath.Join(dir, "agent.yaml")
	require.NoError(t, os.WriteFile(agentFile, []byte(`version: "2"
agents:
  root:
    model: openai/gpt-4o-mini
    instruction: deterministic command bootstrap test
`), 0o600))

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	stdout := newListeningWriter()
	var stderr bytes.Buffer
	cmd := newServeCmd()
	cmd.SetContext(ctx)
	cmd.SetOut(stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"api", agentFile, "--listen", "127.0.0.1:0", "--session-db", filepath.Join(dir, "sessions.db")})

	done := make(chan error, 1)
	go func() { done <- cmd.Execute() }()

	var address string
	select {
	case address = <-stdout.address:
	case <-time.After(10 * time.Second):
		cancel()
		t.Fatalf("serve api did not listen: stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
	baseURL := "http://" + address
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, baseURL+"/api/v2/sessions", strings.NewReader(`{"agent_name":"root","title":"headless"}`))
	require.NoError(t, err)
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	require.NoError(t, err)
	defer response.Body.Close()
	require.Equal(t, http.StatusCreated, response.StatusCode)
	var metadata struct {
		SessionID string `json:"session_id"`
		AgentName string `json:"agent_name"`
	}
	require.NoError(t, json.NewDecoder(response.Body).Decode(&metadata))
	assert.NotEmpty(t, metadata.SessionID)
	assert.Equal(t, "root", metadata.AgentName)

	request, err = http.NewRequestWithContext(t.Context(), http.MethodGet, baseURL+"/api/v2/sessions", http.NoBody)
	require.NoError(t, err)
	catalogResponse, err := http.DefaultClient.Do(request)
	require.NoError(t, err)
	defer catalogResponse.Body.Close()
	require.Equal(t, http.StatusOK, catalogResponse.StatusCode)
	var catalog struct {
		Version  int `json:"version"`
		Sessions []struct {
			SessionID  string `json:"session_id"`
			AgentName  string `json:"agent_name"`
			Attachable bool   `json:"attachable"`
		} `json:"sessions"`
	}
	require.NoError(t, json.NewDecoder(catalogResponse.Body).Decode(&catalog))
	assert.Equal(t, 2, catalog.Version)
	require.Len(t, catalog.Sessions, 1)
	assert.Equal(t, metadata.SessionID, catalog.Sessions[0].SessionID)
	assert.Equal(t, "root", catalog.Sessions[0].AgentName)
	assert.True(t, catalog.Sessions[0].Attachable)

	cancel()
	select {
	case err := <-done:
		require.NoError(t, err, "stderr: "+stderr.String())
	case <-time.After(10 * time.Second):
		t.Fatal("serve api did not exit cleanly after cancellation")
	}
}

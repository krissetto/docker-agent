package api_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/docker/docker-agent/pkg/api"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/stretchr/testify/require"
)

func TestTailSnapshotDTOHasDistinctHeaderAndKeepsFullDefault(t *testing.T) {
	sess := session.New(session.WithID("s"))
	sess.AddMessage(session.UserMessage("secret"))
	full := api.SessionSnapshot[string, string, any]{Session: sess}
	fullJSON, err := json.Marshal(full)
	require.NoError(t, err)
	require.Contains(t, string(fullJSON), "secret")
	tail := full
	tail.Projection = api.SessionHistoryProjection
	tailJSON, err := json.Marshal(tail)
	require.NoError(t, err)
	require.NotContains(t, string(tailJSON), "secret")
	require.NotContains(t, string(tailJSON), `"messages"`)
	require.Contains(t, string(tailJSON), `"projection":"tail"`)
}

func TestTailSnapshotProjectionRemovesMessagesWithoutASecondSessionClone(t *testing.T) {
	// API wire tests run outside package session to avoid import cycles, so use
	// the production runtime snapshot projection to obtain a metadata header.
	sess := session.New(session.WithID("header"))
	header, err := sess.HistoryHeaderSnapshot(4096)
	require.NoError(t, err)
	full := api.SessionSnapshot[string, string, any]{Session: header}
	tail := full
	tail.Projection = api.SessionHistoryProjection
	data, err := json.Marshal(tail)
	require.NoError(t, err)
	require.NotContains(t, string(data), `"messages"`)
}

func TestSnapshotRoundTripsAuthoritativeBackgroundProcesses(t *testing.T) {
	process := tools.BackgroundProcess{ID: "job-1", SessionID: "s", AgentName: "worker", ToolCallID: "call-1", Command: "sleep 1", Cwd: "/tmp", StartedAt: time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC), State: tools.BackgroundProcessRunning}
	wire, err := json.Marshal(api.SessionSnapshot[string, string, any]{Session: session.New(session.WithID("s")), BackgroundProcesses: []tools.BackgroundProcess{process}})
	require.NoError(t, err)
	var decoded api.SessionSnapshot[string, string, any]
	require.NoError(t, json.Unmarshal(wire, &decoded))
	require.Equal(t, []tools.BackgroundProcess{process}, decoded.BackgroundProcesses)
	require.Contains(t, string(wire), `"tool_call_id":"call-1"`)
	require.NotContains(t, string(wire), `"finished_at"`, "running process timestamps must not serialize a phantom zero completion time")
}

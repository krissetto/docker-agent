package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/api"
	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/session"
)

type identityCountingStore struct {
	session.Store
	reads int
}

func (s *identityCountingStore) GetSessions(context.Context) ([]*session.Session, error) {
	s.reads++
	return nil, errors.New("history must not be read")
}
func (s *identityCountingStore) GetSession(context.Context, string) (*session.Session, error) {
	s.reads++
	return nil, errors.New("transcript must not be read")
}
func (s *identityCountingStore) GetSessionByOrigin(context.Context, string, string) (*session.Session, error) {
	s.reads++
	return nil, errors.New("transcript must not be read")
}
func (s *identityCountingStore) GetSessionSummaries(context.Context) ([]session.Summary, error) {
	s.reads++
	return nil, errors.New("history must not be read")
}

type identityProbeStore struct {
	*identityCountingStore
	calls    int
	deadline time.Time
	err      error
}

func (s *identityProbeStore) Ping(ctx context.Context) error {
	s.calls++
	s.deadline, _ = ctx.Deadline()
	return s.err
}

func TestServerIdentityReadinessOnlyUsesBoundedProbe(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{name: "connected", want: true},
		{name: "disconnected", err: errors.New("store disconnected")},
		{name: "timeout", err: context.DeadlineExceeded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			counts := &identityCountingStore{Store: session.NewInMemorySessionStore()}
			probe := &identityProbeStore{identityCountingStore: counts, err: tc.err}
			sm := NewSessionManager(t.Context(), config.Sources{}, probe, 0, &config.RuntimeConfig{})
			srv := NewWithManager(sm, "token")
			before := time.Now()
			rec := sessionRequest(t, srv, http.MethodGet, api.ServerInfoPath, "", "token")
			require.Equal(t, http.StatusOK, rec.Code)
			var info api.ServerInfo
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &info))
			require.Equal(t, tc.want, info.Ready)
			require.Equal(t, 1, probe.calls)
			require.Zero(t, counts.reads)
			require.False(t, probe.deadline.IsZero())
			require.LessOrEqual(t, probe.deadline.Sub(before), time.Second+100*time.Millisecond)
		})
	}
	t.Run("unsupported adapter", func(t *testing.T) {
		counts := &identityCountingStore{Store: session.NewInMemorySessionStore()}
		sm := NewSessionManager(t.Context(), config.Sources{}, counts, 0, &config.RuntimeConfig{})
		srv := NewWithManager(sm, "token")
		rec := sessionRequest(t, srv, http.MethodGet, api.ServerInfoPath, "", "token")
		require.Equal(t, http.StatusOK, rec.Code)
		var info api.ServerInfo
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &info))
		require.False(t, info.Ready)
		require.NotEmpty(t, info.InstanceID)
		require.Zero(t, counts.reads)
	})
}

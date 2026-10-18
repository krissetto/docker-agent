package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/api"
	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
)

func TestSourceAdmissionIsAuthenticatedAndUsesStableIdentity(t *testing.T) {
	factory := &factoryRecorder{store: session.NewInMemorySessionStore()}
	sm := NewSessionManager(t.Context(), config.Sources{"default": config.NewBytesSource("default", []byte("agents: {}"))}, factory.store, 0, &config.RuntimeConfig{}, WithSessionRuntimeFactory(factory.build))
	t.Cleanup(func() { require.NoError(t, sm.Shutdown(context.WithoutCancel(t.Context()))) })
	registrations := 0
	srv := NewWithManager(sm, "secret", WithSourceRegistrar(func(_ context.Context, path, workingDir string) (api.SourceRegistration, config.Source, error) {
		registrations++
		assert.Equal(t, "./team.yaml", path)
		assert.Equal(t, "nested", workingDir)
		return api.SourceRegistration{SourceID: "kit:stable", Revision: "rev-1"}, config.NewBytesSource("/workspace/team.yaml", []byte("agents: {}")), nil
	}))
	body := `{"path":"./team.yaml","working_dir":"nested"}`
	unauthorized := sessionRequest(t, srv, http.MethodPost, api.SourceRegistrationPath, body, "")
	assert.Equal(t, http.StatusUnauthorized, unauthorized.Code)
	assert.Zero(t, registrations)
	authorized := sessionRequest(t, srv, http.MethodPost, api.SourceRegistrationPath, body, "secret")
	require.Equal(t, http.StatusOK, authorized.Code, authorized.Body.String())
	var registration api.SourceRegistration
	require.NoError(t, json.Unmarshal(authorized.Body.Bytes(), &registration))
	assert.Equal(t, api.SourceRegistration{SourceID: "kit:stable", Revision: "rev-1"}, registration)
	_, ok := sm.Source("kit:stable")
	assert.True(t, ok)

	createWithAuth := func(source string) (sessionMetadataDTO, int) {
		rec := sessionRequest(t, srv, http.MethodPost, api.SessionAPIPath, `{"source":"`+source+`","agent_name":"root"}`, "secret")
		var metadata sessionMetadataDTO
		if rec.Code == http.StatusCreated {
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &metadata))
		}
		return metadata, rec.Code
	}
	created, code := createWithAuth("kit:stable")
	require.Equal(t, http.StatusCreated, code)
	stored, err := factory.store.GetSession(t.Context(), created.SessionID)
	require.NoError(t, err)
	assert.Equal(t, "kit:stable", stored.AttributesSnapshot()[sessionSourceAttribute])
	_, code = createWithAuth("another-source")
	assert.Equal(t, http.StatusNotFound, code, "unregistered sources cannot fall back to default")
}

func TestSourceAdmissionAdvancesCurrentSourceGeneration(t *testing.T) {
	factory := &factoryRecorder{store: session.NewInMemorySessionStore()}
	sm := NewSessionManager(t.Context(), config.Sources{"default": config.NewBytesSource("default", []byte("agents: {}"))}, factory.store, 0, &config.RuntimeConfig{}, WithSessionRuntimeFactory(factory.build))
	t.Cleanup(func() { require.NoError(t, sm.Shutdown(context.WithoutCancel(t.Context()))) })
	firstSource := &memorySource{data: "agents: {v: 1}"}
	require.NoError(t, sm.RegisterSource("kit:stable", firstSource))
	srv := NewWithManager(sm, "")
	first, code := createSessionVia(t, srv, `{"source":"kit:stable","agent_name":"root"}`)
	require.Equal(t, http.StatusCreated, code)
	require.Len(t, factory.built(), 1)
	secondSource := &memorySource{data: "agents: {v: 2}"}
	require.NoError(t, sm.RegisterSource("kit:stable", secondSource))
	second, code := createSessionVia(t, srv, `{"source":"kit:stable","agent_name":"root"}`)
	require.Equal(t, http.StatusCreated, code)
	assert.Len(t, factory.built(), 2, "the next session uses a new immutable source generation")
	_, err := sm.Handle(t.Context(), first.SessionID)
	require.NoError(t, err, "the prior resident session remains with its owning runtime")
	_, err = sm.Handle(t.Context(), second.SessionID)
	require.NoError(t, err)
}

type sourceGenerationRecorder struct {
	mu      sync.Mutex
	loaded  []string
	factory *factoryRecorder
}

func (r *sourceGenerationRecorder) build(ctx context.Context, source config.Source, workingDir string) (runtime.SessionRuntimeSupervisor, error) {
	data, err := source.Read(ctx)
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	r.loaded = append(r.loaded, string(data))
	r.mu.Unlock()
	return r.factory.build(ctx, source, workingDir)
}

func (r *sourceGenerationRecorder) last() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.loaded[len(r.loaded)-1]
}

func TestSourceAdmissionColdRestoreUsesCurrentRevision(t *testing.T) {
	store := session.NewInMemorySessionStore()
	firstFactory := &sourceGenerationRecorder{factory: &factoryRecorder{store: store}}
	first := NewSessionManager(t.Context(), config.Sources{"kit:stable": &memorySource{data: "agents: {v: 1}"}}, store, 0, &config.RuntimeConfig{}, WithSessionRuntimeFactory(firstFactory.build))
	created, code := createSessionVia(t, NewWithManager(first, ""), `{"source":"kit:stable","agent_name":"root"}`)
	require.Equal(t, http.StatusCreated, code)
	require.Contains(t, firstFactory.last(), "v: 1")
	require.NoError(t, first.Shutdown(context.WithoutCancel(t.Context())))

	latestFactory := &sourceGenerationRecorder{factory: &factoryRecorder{store: store}}
	latest := NewSessionManager(t.Context(), config.Sources{"kit:stable": &memorySource{data: "agents: {v: 2}"}}, store, 0, &config.RuntimeConfig{}, WithSessionRuntimeFactory(latestFactory.build))
	t.Cleanup(func() { require.NoError(t, latest.Shutdown(context.WithoutCancel(t.Context()))) })
	_, err := latest.Handle(t.Context(), created.SessionID)
	require.NoError(t, err)
	require.Eventually(t, func() bool { return strings.Contains(latestFactory.last(), "v: 2") }, time.Second, time.Millisecond)
}

func TestPublishSourceCommitFailureKeepsActiveGeneration(t *testing.T) {
	store := session.NewInMemorySessionStore()
	factory := &factoryRecorder{store: store}
	sm := NewSessionManager(t.Context(), config.Sources{"kit:stable": &memorySource{data: "agents: {v: 1}"}}, store, 0, &config.RuntimeConfig{}, WithSessionRuntimeFactory(factory.build))
	t.Cleanup(func() { require.NoError(t, sm.Shutdown(context.WithoutCancel(t.Context()))) })
	before, ok := sm.Source("kit:stable")
	require.True(t, ok)
	beforeGeneration := before.(interface{ Generation() uint64 }).Generation()
	err := sm.PublishSourceWithCommit("kit:stable", &memorySource{data: "agents: {v: 2}"}, func() error { return errors.New("injected CAS conflict") })
	require.ErrorContains(t, err, "injected CAS conflict")
	after, ok := sm.Source("kit:stable")
	require.True(t, ok)
	assert.Equal(t, beforeGeneration, after.(interface{ Generation() uint64 }).Generation())
	loaded, err := after.Read(t.Context())
	require.NoError(t, err)
	assert.Contains(t, string(loaded), "v: 1")

	err = sm.PublishSourceWithCommit("kit:new", &memorySource{data: "agents: {}"}, func() error { return errors.New("injected first registration conflict") })
	require.Error(t, err)
	_, ok = sm.Source("kit:new")
	assert.False(t, ok, "failed durable admission must not expose a new source")
}

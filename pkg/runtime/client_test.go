package runtime

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/api"
)

func writeCanonicalObservationStart(w http.ResponseWriter) http.Flusher {
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, "data: {\"version\":%d,\"type\":\"snapshot\",\"snapshot\":{\"session\":{\"id\":\"s\"},\"status\":{\"session_id\":\"s\"},\"cursor\":0,\"transcript_position\":0}}\n\n", api.SessionAPIVersion)
	fmt.Fprintf(w, "data: {\"version\":%d,\"type\":\"ready\",\"cursor\":0}\n\n", api.SessionAPIVersion)
	flusher := w.(http.Flusher)
	flusher.Flush()
	return flusher
}

func TestClientCanonicalObservationDeliversMultipleEvents(t *testing.T) {
	proceed := make(chan struct{}, 2)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		flusher := writeCanonicalObservationStart(w)
		for i := 1; i <= 3; i++ {
			if i > 1 {
				<-proceed
			}
			fmt.Fprintf(w, "data: {\"version\":%d,\"type\":\"event\",\"envelope\":{\"version\":%d,\"session_id\":\"s\",\"sequence\":%d,\"event\":{\"type\":\"session_title\",\"session_id\":\"s\",\"title\":\"t%d\"}}}\n\n", api.SessionAPIVersion, api.SessionAPIVersion, i, i)
			flusher.Flush()
		}
	}))
	defer srv.Close()
	client, err := NewClient(srv.URL, WithHTTPClient(srv.Client()))
	require.NoError(t, err)
	transport, err := NewSessionTransport(client)
	require.NoError(t, err)
	handle, err := transport.SessionByID("s")
	require.NoError(t, err)
	observation, err := handle.Observe(t.Context(), ObserveOptions{})
	require.NoError(t, err)
	defer observation.Cancel()
	var titles []string
	for envelope := range observation.Events {
		titles = append(titles, envelope.Event.(*SessionTitleEvent).Title)
		if len(titles) < 3 {
			proceed <- struct{}{}
		}
	}
	assert.Equal(t, []string{"t1", "t2", "t3"}, titles)
}

func TestClientCanonicalObservationStopsWhenContextCancelled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher := writeCanonicalObservationStart(w)
		<-r.Context().Done()
		flusher.Flush()
	}))
	defer srv.Close()
	client, err := NewClient(srv.URL, WithHTTPClient(srv.Client()))
	require.NoError(t, err)
	transport, err := NewSessionTransport(client)
	require.NoError(t, err)
	handle, err := transport.SessionByID("s")
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	observation, err := handle.Observe(ctx, ObserveOptions{})
	require.NoError(t, err)
	cancel()
	select {
	case _, ok := <-observation.Events:
		assert.False(t, ok)
	case <-time.After(time.Second):
		t.Fatal("observation did not close after cancellation")
	}
}

func TestClientGetAgentPreservesFullSourceIdentityAsPathSegment(t *testing.T) {
	for _, source := range []string{"/workspace/team.yaml", "/workspace/team with space%#.yaml"} {
		t.Run(source, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, "/prefix/api/agents/"+url.PathEscape(source), r.URL.EscapedPath())
				encoded := strings.TrimPrefix(r.URL.EscapedPath(), "/prefix/api/agents/")
				decoded, err := url.PathUnescape(encoded)
				assert.NoError(t, err)
				assert.Equal(t, source, decoded)
				assert.Empty(t, r.URL.RawQuery)
				fmt.Fprint(w, `{}`)
			}))
			defer server.Close()
			client, err := NewClient(server.URL + "/prefix")
			require.NoError(t, err)
			_, err = client.GetAgent(t.Context(), source)
			require.NoError(t, err)
		})
	}
}

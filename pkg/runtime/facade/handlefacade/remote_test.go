package handlefacade_test

import (
	"context"
	"net"
	"testing"

	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/runtime/facade"
	"github.com/docker/docker-agent/pkg/runtime/facade/facadetest"
	"github.com/docker/docker-agent/pkg/runtime/facade/handlefacade"
	"github.com/docker/docker-agent/pkg/server"
	"github.com/docker/docker-agent/pkg/session"
)

// newRemoteFixture serves a real local runtime through the production HTTP server
// and returns a facade host that reaches it only through the remote transport.
func newRemoteFixture(t *testing.T, cfg facadetest.Config) facadetest.Fixture {
	t.Helper()
	store := session.NewInMemorySessionStore()
	harness := facadetest.NewHarness(t, cfg, store)
	local := harness.Runtime
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	manager := server.NewSessionManager(ctx, config.Sources{}, store, 0, &config.RuntimeConfig{}, server.WithSessionRuntime(local))
	srv := server.NewWithManager(manager, "")
	listener, err := (&net.ListenConfig{}).Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve(ctx, listener) }()

	client, err := runtime.NewClient("http://" + listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	transport, err := runtime.NewSessionTransport(client)
	if err != nil {
		t.Fatal(err)
	}
	host, err := handlefacade.NewHost(transport)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = host.Close(context.WithoutCancel(ctx)) })
	return facadetest.Fixture{Host: host, Model: harness.Model, Worker: harness.Worker}
}

func TestConformanceRemote(t *testing.T) {
	facadetest.Run(t, newRemoteFixture)
}

// TestRemoteCapabilityMatrixMatchesLocal pins the remote handle's capability set
// to the local one. The only difference is AgentSwitch: switching in place is a
// local-only operation, so the remote handle must not advertise it.
func TestRemoteCapabilityMatrixMatchesLocal(t *testing.T) {
	local := newLocalFixture(t, facadetest.Config{})
	remote := newRemoteFixture(t, facadetest.Config{})
	localRoot, err := local.Host.Create(t.Context(), facade.CreateOptions{AgentName: "root"})
	if err != nil {
		t.Fatal(err)
	}
	remoteRoot, err := remote.Host.Create(t.Context(), facade.CreateOptions{AgentName: "root"})
	if err != nil {
		t.Fatal(err)
	}
	want, got := localRoot.Info().Capabilities, remoteRoot.Info().Capabilities
	if !want.AgentSwitch {
		t.Fatal("the local handle must advertise agent switching")
	}
	if got.AgentSwitch {
		t.Fatal("the remote handle advertises agent switching it cannot perform")
	}
	want.AgentSwitch = false
	if want != got {
		t.Fatalf("remote capabilities differ from local:\nlocal:  %+v\nremote: %+v", want, got)
	}
	_, switchable := remoteRoot.AgentSwitch()
	if switchable {
		t.Fatal("the remote AgentSwitch accessor must report absence")
	}
}

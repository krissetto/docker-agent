package handlefacade_test

import (
	"context"
	"testing"

	"github.com/docker/docker-agent/pkg/runtime/facade/facadetest"
	"github.com/docker/docker-agent/pkg/runtime/facade/handlefacade"
)

// newLocalFixture serves an in-process runtime through the handle adapter.
func newLocalFixture(t *testing.T, cfg facadetest.Config) facadetest.Fixture {
	t.Helper()
	harness := facadetest.NewLocalRuntime(t, cfg)
	host, err := handlefacade.NewHost(harness.Runtime)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = host.Close(context.WithoutCancel(t.Context())) })
	return facadetest.Fixture{Host: host, Model: harness.Model, Worker: harness.Worker}
}

func TestConformance(t *testing.T) {
	facadetest.Run(t, newLocalFixture)
}

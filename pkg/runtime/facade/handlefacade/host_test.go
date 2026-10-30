package handlefacade_test

import (
	"context"
	"errors"
	"testing"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/runtime/facade"
	"github.com/docker/docker-agent/pkg/runtime/facade/facadetest"
	"github.com/docker/docker-agent/pkg/runtime/facade/handlefacade"
)

// catalogOnlyRegistry hides the summary catalog and exposes only the session catalog.
type catalogOnlyRegistry struct {
	runtime.SessionRuntime

	catalog runtime.SessionCatalog
}

func (r catalogOnlyRegistry) ListSessions(ctx context.Context) ([]runtime.SessionCatalogEntry, error) {
	return r.catalog.ListSessions(ctx)
}

func TestNewHostRefusesRegistryWithoutAnyCatalog(t *testing.T) {
	t.Parallel()
	rt := facadetest.NewLocalRuntime(t, facadetest.Config{}).Runtime
	// Embedding only the interface hides every catalog method.
	_, err := handlefacade.NewHost(struct{ runtime.SessionRuntime }{rt})
	if !errors.Is(err, handlefacade.ErrNoSessionCatalog) {
		t.Fatalf("NewHost error = %v, want ErrNoSessionCatalog", err)
	}
}

func TestListFallsBackToSessionCatalog(t *testing.T) {
	t.Parallel()
	rt := facadetest.NewLocalRuntime(t, facadetest.Config{}).Runtime
	catalog, ok := rt.(runtime.SessionCatalog)
	if !ok {
		t.Fatal("the local registry must expose the session catalog")
	}
	host, err := handlefacade.NewHost(catalogOnlyRegistry{SessionRuntime: rt, catalog: catalog})
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	sess, err := host.Create(ctx, facade.CreateOptions{AgentName: "root"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sess.Close(context.WithoutCancel(ctx)) })

	page, err := host.List(ctx, facade.ListOptions{})
	if err != nil {
		t.Fatalf("List through the fallback: %v", err)
	}
	for _, summary := range page.Sessions {
		if summary.SessionID == sess.Info().SessionID {
			return
		}
	}
	t.Fatalf("created session %s is missing from the fallback listing", sess.Info().SessionID)
}

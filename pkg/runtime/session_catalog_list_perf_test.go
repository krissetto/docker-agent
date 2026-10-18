package runtime

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/session/sqlitestore"
	"github.com/docker/docker-agent/pkg/team"
)

func BenchmarkLocalSessionCatalogLargeSubagentHistory(b *testing.B) {
	ctx := context.Background()
	store, err := sqlitestore.New(ctx, filepath.Join(b.TempDir(), "sessions.db"))
	if err != nil {
		b.Fatal(err)
	}
	for rootIndex := range 3 {
		root := session.New(session.WithID(fmt.Sprintf("root-%d", rootIndex)), session.WithAgentName("root"))
		root.SetAttribute(SessionAgentAttribute, "root")
		for childIndex := range 100 {
			child := session.New(session.WithID(fmt.Sprintf("root-%d-child-%03d", rootIndex, childIndex)), session.WithParentID(root.ID), session.WithAgentName("worker"))
			child.SetAttribute(SessionAgentAttribute, "worker")
			for messageIndex := range 12 {
				child.AddMessage(session.NewAgentMessage("worker", &chat.Message{Role: chat.MessageRoleUser, Content: fmt.Sprintf("root %d child %d message %d deterministic fixture", rootIndex, childIndex, messageIndex)}))
			}
			root.AddSubSession(child)
		}
		if err := store.AddSession(ctx, root); err != nil {
			b.Fatal(err)
		}
	}
	rt, err := NewLocalRuntime(ctx, team.New(team.WithAgents(agent.New("root", "", agent.WithModel(&mockProvider{id: "test/root"})), agent.New("worker", "", agent.WithModel(&mockProvider{id: "test/worker"})))), WithSessionStore(store), WithSessionCompaction(false))
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = rt.shutdownSessions(ctx); _ = store.Close() })
	catalog := NewSessionRuntimeSupervisor(rt).Runtime().(SessionCatalog)
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		rows, err := catalog.ListSessions(ctx)
		if err != nil {
			b.Fatal(err)
		}
		if len(rows) != 3 {
			b.Fatalf("got %d roots", len(rows))
		}
	}
}

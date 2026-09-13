package session

import (
	"sync"
	"testing"

	"github.com/docker/docker-agent/pkg/chat"
)

// TestInMemoryStoreAddMessageConcurrentUniqueIDs pins the fix for
// InMemorySessionStore.messageID: it used to be a plain int64 incremented
// with `s.messageID++`, which loses increments under concurrent AddMessage
// calls (HTTP handlers racing the runtime's PersistenceObserver) and can
// hand out duplicate IDs. A duplicate ID makes UpdateMessage (which looks
// up messages by ID) edit the wrong message. The counter must be an
// atomic.Int64 incremented via Add(1) so every concurrent AddMessage call
// observes a unique, monotonically increasing ID.
func TestInMemoryStoreAddMessageConcurrentUniqueIDs(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store := NewInMemorySessionStore()
	sess := New()
	if err := store.AddSession(ctx, sess); err != nil {
		t.Fatalf("AddSession: %v", err)
	}

	const n = 200
	ids := make([]int64, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			id, err := store.AddMessage(ctx, sess.ID, &Message{Message: chat.Message{Role: chat.MessageRoleUser, Content: "u"}})
			if err != nil {
				t.Errorf("AddMessage: %v", err)
				return
			}
			ids[i] = id
		})
	}
	wg.Wait()

	seen := make(map[int64]bool, n)
	for _, id := range ids {
		if id == 0 {
			t.Fatalf("got zero message ID, AddMessage call must have failed")
		}
		if seen[id] {
			t.Fatalf("duplicate message ID %d assigned to two concurrent AddMessage calls", id)
		}
		seen[id] = true
	}
	if len(seen) != n {
		t.Fatalf("expected %d unique message IDs, got %d", n, len(seen))
	}

	stored, err := store.GetSession(ctx, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := stored.MessageCount(); got != n {
		t.Errorf("expected %d messages recorded on the session, got %d", n, got)
	}
}

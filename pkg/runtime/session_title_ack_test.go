package runtime

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/sessiontitle"
)

type titleAcknowledgementStore struct {
	session.Store

	entered chan struct{}
	release chan struct{}
	failure error
}

func (s *titleAcknowledgementStore) UpdateSessionTitle(ctx context.Context, id, title string) error {
	close(s.entered)
	<-s.release
	if s.failure != nil {
		return s.failure
	}
	return s.Store.UpdateSessionTitle(context.WithoutCancel(ctx), id, title)
}

func TestSessionTitleReservedWriteJoinsSuccessAndErrorAfterCancellation(t *testing.T) {
	for _, failure := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "error"}[failure], func(t *testing.T) {
			p := &ownedTitleProvider{}
			store := &titleAcknowledgementStore{Store: session.NewInMemorySessionStore(), entered: make(chan struct{}), release: make(chan struct{})}
			if failure {
				store.failure = errors.New("write failed")
			}
			h := titleOwnerFixture(t, p)
			h.runtime.sessionStore = store
			snapshot, err := h.Snapshot(t.Context())
			require.NoError(t, err)
			require.NoError(t, store.AddSession(t.Context(), snapshot))
			require.NoError(t, h.GenerateSessionTitle(t.Context(), sessiontitle.New(p), []string{"hello"}, false))
			<-store.entered
			require.NoError(t, h.driver.ownerCall(t.Context(), func() error { h.driver.invalidateTitleLocked(); return nil }))
			require.Equal(t, "started", titleStatus(t, h), "an accepted durable write remains pending until its acknowledgement")
			replacement := snapshot.Clone()
			replacement.SetTitle("replacement")
			require.False(t, h.ReplaceSettledSession(replacement), "write reservation fences replacement until joined acknowledgement")
			close(store.release)
			h.driver.wg.Wait()
			want := "completed"
			if failure {
				want = "failed"
			}
			require.Equal(t, want, titleStatus(t, h))
			view, err := h.Snapshot(t.Context())
			require.NoError(t, err)
			persisted, err := store.GetSession(t.Context(), h.ID())
			require.NoError(t, err)
			require.Equal(t, persisted.TitleSnapshot(), view.TitleSnapshot())
			if failure {
				require.Empty(t, view.TitleSnapshot())
			} else {
				require.Equal(t, "Owned title", view.TitleSnapshot())
			}
		})
	}
}

package session

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSummaryPagesBoundedStableCursorAndScope(t *testing.T) {
	for name, create := range map[string]func() Store{"memory": NewInMemorySessionStore, "sqlite": func() Store { return openMemoryStore(t) }} {
		t.Run(name, func(t *testing.T) {
			store := create()
			paged := store.(PagedSummaryStore)
			for i := range 9 {
				sess := New(WithID(fmt.Sprintf("s%d", i)))
				sess.CreatedAt = time.Unix(100-int64(i/3), 0).UTC()
				if i == 8 {
					sess.ParentID = "s0"
				}
				require.NoError(t, store.AddSession(t.Context(), sess))
			}
			for _, includeChildren := range []bool{false, true} {
				options := SummaryPageOptions{Limit: 2, IncludeChildren: includeChildren}
				var ids []string
				for {
					page, err := paged.GetSessionSummaryPage(t.Context(), options)
					require.NoError(t, err)
					assert.LessOrEqual(t, len(page.Summaries), 2)
					for _, summary := range page.Summaries {
						ids = append(ids, summary.ID)
					}
					if !page.HasMore {
						break
					}
					last := page.Summaries[len(page.Summaries)-1]
					options.AfterCreatedAt, options.AfterID = last.CreatedAt, last.ID
				}
				want := []string{"s0", "s1", "s2", "s3", "s4", "s5", "s6", "s7"}
				if includeChildren {
					want = append(want, "s8")
				}
				assert.Equal(t, want, ids)
			}
		})
	}
}

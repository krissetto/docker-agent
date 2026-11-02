package messages

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/service"
)

// vprlSizes are persisted session-item counts. Every item is one real message
// loaded through the production replay path, never streamed chunks.
var vprlSizes = []int{100, 1000, 6000}

// vprlAnchorSlack is the allowance on top of the visible item count that every
// render bound below admits: the anchor item plus one item of fill.
const vprlAnchorSlack = 2

// vprlRealSession returns exactly n session items: alternating user prompts and
// assistant answers. Every seventh assistant answer is a long markdown body of
// about 1,000 words so wrapping and height changes touch heavy content.
func vprlRealSession(tb testing.TB, n int) *session.Session {
	tb.Helper()
	sess := session.New(session.WithID("vprl-real"))
	long := strings.Repeat("word ", 996) + "**bold** `code` λ界 end"
	for i := range n {
		if i%2 == 0 {
			sess.AddMessage(session.UserMessage(fmt.Sprintf("question %d about the transcript relayout and viewport", i)))
			continue
		}
		body := fmt.Sprintf("## Answer %d\n\nShort paragraph with **bold**, `code` and [a link](https://example.com/%d).\n\n- item one\n- item two\n", i, i)
		if (i/2)%7 == 6 {
			body = fmt.Sprintf("## Long answer %d\n\n%s\n", i, long)
		}
		sess.AddMessage(session.NewAgentMessage("root", &chat.Message{Role: chat.MessageRoleAssistant, Content: body}))
	}
	require.Len(tb, sess.Messages, n, "fixture must hold exactly n session items")
	return sess
}

// vprlLoad loads sess through the production replay path (BeginReplay, then the
// recent-first history paging drained to completion) and renders once. Callers
// own cleanup: register t.Cleanup(m.StopAnimations) when the model is kept.
func vprlLoad(tb testing.TB, width int, sess *session.Session) *model {
	tb.Helper()
	m := NewScrollableView(animation.NewRuntime(), width, 40, &service.SessionState{}).(*model)
	BeginReplay(m, PrepareReplay(sess), nil)
	drainRetainedReplay(tb, m)
	finishHistoryReplay(tb, m)
	_ = m.View()
	return m
}

// vprlRenders is the event-loop-owned count of message bodies rendered so far.
func vprlRenders(m *model) uint64 {
	_, _, renders := m.ResizeCacheStats()
	return renders
}

// vprlVisibleItems counts the view slots whose rows intersect the viewport. It
// uses only displayEnd and the viewport fields, which both the parent and the
// candidate keep exact. The parent's lineOffsets facade is not used: in the
// candidate it is stale for unmaterialized slots, so counting from it made
// nearly every slot look visible. Slot i spans [start(i), displayEnd(i)), and
// both bounds are nondecreasing in i, so two binary searches find the range.
func vprlVisibleItems(m *model) int {
	n := len(m.views)
	top, bottom := m.scrollOffset, m.scrollOffset+m.height
	if n == 0 || m.height <= 0 {
		return 0
	}
	start := func(i int) int {
		if i == 0 {
			return 0
		}
		return m.displayEnd(i - 1)
	}
	first := sort.Search(n, func(i int) bool { return m.displayEnd(i) > top })
	past := sort.Search(n, func(i int) bool { return start(i) >= bottom })
	// Folded work children have zero own rows, so only slots with positive
	// height count. The loop covers the slot span of the viewport, not history.
	count := 0
	for i := first; i < past; i++ {
		if m.displayEnd(i) > start(i) {
			count++
		}
	}
	return count
}

// vprlFixture is one transcript composition used by the matrix and benchmarks.
type vprlFixture struct {
	name  string
	build func(tb testing.TB, n int) *session.Session
}

// vprlFixtures returns the two compositions. "mixed" is vprlRealSession: about
// one assistant answer in seven is a long synthetic answer. "heavy" makes every
// assistant answer the real 1,121-word markdown document from the markdown
// package's streaming benchmark (headings, lists, code, tables).
func vprlFixtures() []vprlFixture {
	return []vprlFixture{
		{name: "mixed", build: vprlRealSession},
		{name: "heavy", build: vprlHeavySession},
	}
}

var (
	vprlHeavyOnce sync.Once
	vprlHeavyBody string
	errVprlHeavy  error
)

// vprlHeavyDoc returns the real long-answer document, read once per process.
func vprlHeavyDoc(tb testing.TB) string {
	tb.Helper()
	vprlHeavyOnce.Do(func() {
		data, err := os.ReadFile("../markdown/testdata/streaming_benchmark.md")
		vprlHeavyBody, errVprlHeavy = string(data), err
	})
	require.NoError(tb, errVprlHeavy)
	return vprlHeavyBody
}

// vprlHeavySession returns exactly n session items: alternating user prompts
// and assistant answers, where every assistant answer is the real long document
// under a per-item heading.
func vprlHeavySession(tb testing.TB, n int) *session.Session {
	tb.Helper()
	doc := vprlHeavyDoc(tb)
	sess := session.New(session.WithID("vprl-heavy"))
	for i := range n {
		if i%2 == 0 {
			sess.AddMessage(session.UserMessage(fmt.Sprintf("question %d about the heavy transcript relayout", i)))
			continue
		}
		body := fmt.Sprintf("## Heavy answer %d\n\n%s", i, doc)
		sess.AddMessage(session.NewAgentMessage("root", &chat.Message{Role: chat.MessageRoleAssistant, Content: body}))
	}
	require.Len(tb, sess.Messages, n, "heavy fixture must hold exactly n session items")
	return sess
}

// vprlContentBytes sums the message content bytes of a session, for disclosure.
func vprlContentBytes(sess *session.Session) int {
	total := 0
	for _, item := range sess.Messages {
		if item.IsMessage() {
			total += len(item.Message.Message.Content)
		}
	}
	return total
}

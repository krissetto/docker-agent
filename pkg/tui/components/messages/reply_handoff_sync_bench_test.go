package messages

import (
	"fmt"
	"strings"
	"testing"

	"github.com/docker/docker-agent/pkg/tui/animation"
	msgtypes "github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/types"
)

// handoffSyncFixture holds an active reply handoff over multi-line replies, so one
// sync pass touches many rows. Baseline and post-memo runs use this same fixture.
func handoffSyncFixture(tb testing.TB) *model {
	tb.Helper()
	ar := animation.NewRuntimeWithScheduler(&replyHandoffScheduler{now: testHandoffTime()})
	m := NewScrollableView(ar, 116, 40, &service.SessionState{}).(*model)
	tb.Cleanup(func() { m.StopAnimations(); ar.Stop() })
	m.SetCompactWorkLog(true)
	m.SetVisibleReplies(msgtypes.VisibleRepliesThree)
	m.suppressReplyHandoff = true
	m.AddUserMessage("request")
	for i := range 3 {
		m.addMessage(types.Agent(types.MessageTypeAssistant, "root", strings.Repeat(fmt.Sprintf("seed %d prose line\n", i), 24)))
		m.committedMessageCount = len(m.messages)
	}
	m.suppressReplyHandoff = false
	m.View()
	m.addMessage(types.Agent(types.MessageTypeAssistant, "root", strings.Repeat("fourth reply prose line\n", 24)))
	m.View()
	return m
}

// BenchmarkReplyHandoffSyncPass times one per-tick sync pass during an active handoff.
func BenchmarkReplyHandoffSyncPass(b *testing.B) {
	m := handoffSyncFixture(b)
	if !m.replyHandoff.Active() {
		b.Fatal("fixture must hold an active reply handoff")
	}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		m.syncAnimatedWorkProjection()
	}
}

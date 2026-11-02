package tui

import (
	"fmt"
	"os"
	"runtime"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	chatmsg "github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tui/page/chat"
)

// vprlRootHistory returns n real session items for the root harness, using the
// same shape as the messages-level fixture: alternating prompts and answers,
// with a long answer every seventh assistant item.
func vprlRootHistory(n int) []session.Item {
	long := strings.Repeat("word ", 996) + "**bold** `code` λ界 end"
	items := make([]session.Item, 0, n)
	for i := range n {
		if i%2 == 0 {
			items = append(items, session.NewMessageItem(session.UserMessage(fmt.Sprintf("question %d about the transcript relayout", i))))
			continue
		}
		body := fmt.Sprintf("## Answer %d\n\nShort paragraph with **bold**, `code` and a link.\n\n- item one\n- item two\n", i)
		if (i/2)%7 == 6 {
			body = fmt.Sprintf("## Long answer %d\n\n%s\n", i, long)
		}
		items = append(items, session.NewMessageItem(&session.Message{AgentName: "root", Message: chatmsg.Message{Role: chatmsg.MessageRoleAssistant, Content: body}}))
	}
	return items
}

// TestActualProgramViewportRelayoutRealMouseAndResize drives a real bubbletea
// program over the production root with real session items. It uses real
// wheel events, a real scrollbar press/drag/release, and a real resize. The
// scrollbar is measured before the program starts, so the test goroutine never
// reads root state while the program runs.
func TestActualProgramViewportRelayoutRealMouseAndResize(t *testing.T) {
	sizes := []int{100, 1000}
	if os.Getenv("VPRL_HEAVY") != "" {
		sizes = append(sizes, 6000)
	}
	for _, n := range sizes {
		t.Run(fmt.Sprintf("messages=%d", n), func(t *testing.T) {
			root, _, _ := wallClockRoot(t, 120, 40)
			root.application.Session().Messages = vprlRootHistory(n)
			_ = root.chatPage.Init()
			root.handleWindowResize(120, 40)
			root.chatPage.ScrollToBottom()
			_ = root.View()
			page := root.chatPage.(chat.SplitPresentation)
			thumb := page.MeasureSplitShell(root.width, root.contentHeight).TranscriptArea
			x, bottomY, topY := thumb.X+thumb.Width-1, thumb.Y+thumb.Height-1, thumb.Y

			model := &streamingMotionModel{root: root, ready: make(chan struct{})}
			program := startStreamingMotionProgram(t, model, tea.WithOutput(&wallClockCountingWriter{}))
			programAck(t, program)
			bottom := programFrame(t, program)
			require.NotEmpty(t, strings.TrimSpace(ansi.Strip(bottom)))

			program.Send(tea.MouseClickMsg{X: x, Y: bottomY, Button: tea.MouseLeft})
			const steps = 8
			for step := 1; step <= steps; step++ {
				y := bottomY + (topY-bottomY)*step/steps
				program.Send(tea.MouseMotionMsg{X: x, Y: y, Button: tea.MouseLeft})
			}
			program.Send(tea.MouseReleaseMsg{X: x, Y: topY, Button: tea.MouseLeft})
			programAck(t, program)
			dragged := programFrame(t, program)
			require.NotEqual(t, bottom, dragged, "real scrollbar drag moves the transcript")

			programAck(t, program)
			require.Equal(t, dragged, programFrame(t, program), "release leaves the drag position settled")

			for range 3 {
				program.Send(tea.MouseWheelMsg{X: 40, Y: 20, Button: tea.MouseWheelDown})
			}
			programAck(t, program)
			wheeled := programFrame(t, program)
			require.NotEqual(t, dragged, wheeled, "real wheel down moves the transcript")

			program.Send(tea.WindowSizeMsg{Width: 90, Height: 40})
			programAck(t, program)
			resized := programFrame(t, program)
			require.NotEmpty(t, strings.TrimSpace(ansi.Strip(resized)), "resized viewport is not blank")
			require.Equal(t, 90, ansi.StringWidth(strings.Split(resized, "\n")[0]), "resized frame uses the new width")
		})
	}
}

// vprlC2History is the parity fixture for the opt-in 6k actual-message fixture
// on the C2 branch (scroll-fade-hover-bar, root_message_fixture_test.go,
// messageHistorySession): alternating user prompts and two-paragraph answers,
// with the same text.
func vprlC2History(n int) []session.Item {
	items := make([]session.Item, 0, n)
	for i := range n {
		var msg *session.Message
		if i%2 == 0 {
			msg = session.UserMessage(fmt.Sprintf("question %d about the transcript scroll fade and hover bar", i))
		} else {
			msg = &session.Message{AgentName: "root", Message: chatmsg.Message{Role: chatmsg.MessageRoleAssistant, Content: fmt.Sprintf("answer %d: first paragraph with **bold**, `code` and wrapping text for the viewport width.\n\nsecond paragraph %d closes the answer.", i, i)}}
		}
		items = append(items, session.NewMessageItem(msg))
	}
	return items
}

// vprlHeapAfterGC returns the live heap after two full collections.
func vprlHeapAfterGC() uint64 {
	runtime.GC()
	runtime.GC()
	var stats runtime.MemStats
	runtime.ReadMemStats(&stats)
	return stats.HeapAlloc
}

// TestActualRootViewportRelayoutRetainedMemoryReport logs the heap retained
// after GC by the root with a loaded history of real session items, measured
// from a baseline taken after the harness exists. Gated: VPRL_MEMORY=1. Each
// size runs as its own subtest, so earlier roots are released before the next.
func TestActualRootViewportRelayoutRetainedMemoryReport(t *testing.T) {
	if os.Getenv("VPRL_MEMORY") == "" {
		t.Skip("set VPRL_MEMORY=1 to run the root retained-memory report")
	}
	fixtures := []struct {
		name  string
		items func(int) []session.Item
	}{{"mixed", vprlRootHistory}, {"c2-parity", vprlC2History}}
	for _, fx := range fixtures {
		for _, n := range []int{100, 1000, 6000} {
			t.Run(fmt.Sprintf("fixture=%s/messages=%d", fx.name, n), func(t *testing.T) {
				root, _, _ := wallClockRoot(t, 120, 40)
				items := fx.items(n)
				content := 0
				for _, item := range items {
					content += len(item.Message.Message.Content)
				}
				before := vprlHeapAfterGC()
				root.application.Session().Messages = items
				_ = root.chatPage.Init()
				root.handleWindowResize(120, 40)
				root.chatPage.ScrollToBottom()
				_ = root.View()
				root.handleWindowResize(90, 40)
				_ = root.View()
				after := vprlHeapAfterGC()
				retained := int64(after) - int64(before)
				t.Logf("root memory fixture=%s messages=%d content_bytes=%d retained_bytes=%d retained_bytes_per_item=%.0f retained_per_content_byte=%.2f",
					fx.name, n, content, retained, float64(retained)/float64(n), float64(retained)/float64(max(1, content)))
				runtime.KeepAlive(root)
			})
		}
	}
}

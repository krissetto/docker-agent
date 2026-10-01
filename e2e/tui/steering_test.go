// Steering end-to-end scenarios (issue #3547): messages sent while the agent
// is streaming queue for a later turn by default, and the /settings
// Behavior tab can opt into steering them into the ongoing stream instead.
//
// Both tests replay their cassette through the proxy's simulated-stream mode:
// the first answer streams one character per chunk with a real delay, so the
// test has a wide, deterministic window to interact with the TUI mid-stream.

package tui_test

import (
	"bytes"
	"context"
	"os"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/fake"
	agentruntime "github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/tuitest"
	"github.com/docker/docker-agent/pkg/userconfig"
)

// steeringProxyOptions slows the SSE replay enough that submitting a
// follow-up while the first answer is still streaming is deterministic even
// on slow CI and under -race (the first answer streams one character per
// chunk, so the window is about 4 seconds wide).
func steeringProxyOptions() *fake.ProxyOptions {
	return &fake.ProxyOptions{
		SimulateStream:   true,
		StreamChunkDelay: 250 * time.Millisecond,
	}
}

// newStreamingTUI builds the steering harness and, on Windows only, raises
// the WaitFor deadline to 30s: the simulated stream replays slower there
// (issue #3983). Applied after construction because newTUIWithProxyOptions
// does not forward tuitest options; it is safe before any interaction.
func newStreamingTUI(t *testing.T, options ...*fake.ProxyOptions) *tuitest.Driver {
	t.Helper()
	proxyOptions := steeringProxyOptions()
	if len(options) != 0 {
		proxyOptions = options[0]
	}
	d := newTUIWithProxyOptions(t, "testdata/basic.yaml", 120, 40, proxyOptions)
	if runtime.GOOS == "windows" {
		tuitest.WithTimeout(30 * time.Second)(d)
	}
	return d
}

// TestChat_SteerWhileStreaming submits a second message while the agent is
// still streaming its first answer. The message must be steered into the
// ongoing stream: the steering toast appears, no queue toast is shown, and
// once the runtime drains the message the transcript shows the injected user
// bubble followed by the agent's answer to it.
func TestChat_SteerWhileStreaming(t *testing.T) {
	options := steeringProxyOptions()
	gateEntered, release := make(chan struct{}), make(chan struct{})
	var gateOnce sync.Once
	var prefixSent atomic.Bool
	options.BeforeStreamChunk = func(ctx context.Context, chunk []byte) {
		if !bytes.Contains(chunk, []byte(`"id":"chatcmpl-steer-a1"`)) {
			return
		}
		if prefixSent.Load() {
			gateOnce.Do(func() { close(gateEntered) })
			select {
			case <-ctx.Done():
			case <-release:
			}
			return
		}
		if bytes.Contains(chunk, []byte(`"content":"+"`)) {
			prefixSent.Store(true)
		}
	}
	d := newStreamingTUI(t, options)
	t.Cleanup(func() { close(release) })
	openBehaviorSettings(d).
		Press(tea.KeyRight).
		WaitFor(tuitest.Contains("‹ Steer ›"))
	saveSettingsAndWait(d)

	// Draft the follow-up as a single paste so it costs one Update instead of
	// one per keystroke (keystrokes are expensive under -race and would eat
	// into the streaming window). Submission waits until chunks are visibly
	// streaming.
	d.Type("What's 2+2?").
		Enter().
		Send(tea.PasteMsg{Content: "Also, what's 3+3?"}).
		WaitFor(tuitest.Contains("What's 2+2?")).
		WaitFor(tuitest.Contains("2 +"))
	firstBeforeSteer := d.Frame()
	select {
	case <-gateEntered:
	case <-time.After(10 * time.Second):
		t.Fatal("first response did not reach the replay gate")
	}

	// Plain Enter with explicit Steer mode injects into the ongoing stream.
	d.Enter().
		WaitFor(tuitest.Contains("Message sent to the working agent")).
		Assert(tuitest.Absent("Message queued"))
	// The first answer remains in the session history sent to the second model
	// call, even if the renderer coalesces adjacent assistant blocks.
	require.Contains(t, firstBeforeSteer, "2 +")
	d.WaitFor(tuitest.Contains("Also, what's 3+3?")).
		WaitFor(tuitest.Contains("3 + 3 equals 6.")).
		Assert(tuitest.Absent("2 + 2 equals 4."))
}

// TestChat_QueueSendModeWhileStreaming switches the send mode to Queue via
// the /settings dialog, then submits a second message while the agent is
// still streaming. The session must accept it immediately, project it in the
// sidebar, and promote it only once the first stream stops, producing a second
// turn with its own answer. The switch must also be persisted to user config.
func TestChat_QueueSendModeWhileStreaming(t *testing.T) {
	accepted, promoted := make(chan queuedIdentity, 1), make(chan queuedIdentity, 1)
	d := newTUIWithProxyOptionsWrapped(t, "testdata/basic.yaml", 120, 40, steeringProxyOptions(), func(model tea.Model) tea.Model {
		return &queuedInputObserver{Model: model, accepted: accepted, promoted: promoted}
	})
	if runtime.GOOS == "windows" {
		tuitest.WithTimeout(30 * time.Second)(d)
	}

	// Start with an explicit steering preference, then restore the default.
	openBehaviorSettings(d).
		Press(tea.KeyRight).
		WaitFor(tuitest.Contains("‹ Steer ›"))
	saveSettingsAndWait(d)

	// Restore Queue through the real draft and focused Save action.
	openBehaviorSettings(d).
		Press(tea.KeyRight).
		WaitFor(tuitest.Contains("‹ Queue ›"))
	saveSettingsAndWait(d)

	// The choice is persisted for future sessions.
	cfg, err := os.ReadFile(userconfig.Path())
	require.NoError(t, err)
	assert.NotContains(t, string(cfg), "busy_send_mode:", "queue is the omitted default")

	d.Type("What's 2+2?").
		Enter().
		Send(tea.PasteMsg{Content: "Also, what's 3+3?"}).
		WaitFor(tuitest.Contains("What's 2+2?")).
		WaitFor(tuitest.Contains("2 +"))

	// With queue send mode, session admission immediately projects the pending
	// FIFO in the sidebar instead of maintaining a local queue/toast mirror.
	d.Enter().
		WaitFor(tuitest.Matches(`(?m)^ {40,}- Also, what's 3\+3\?\s*$`)).
		WaitFor(tuitest.Contains("Also, what's 3+3?"))
	d.Assert(tuitest.Absent("Message sent to the working agent"))
	require.Equal(t, 1, strings.Count(d.Frame(), "Also, what's 3+3?"), "exactly one pending FIFO row")
	var admitted queuedIdentity
	select {
	case admitted = <-accepted:
		require.NotEmpty(t, admitted.turnID)
	case <-time.After(10 * time.Second):
		t.Fatal("canonical pending admission was not observed")
	}

	// Promotion removes it from the sidebar and renders it once in chat.
	d.WaitFor(tuitest.Contains("2 + 2 equals 4.")).
		WaitFor(tuitest.Contains("Also, what's 3+3?")).
		WaitFor(tuitest.Contains("3 + 3 equals 6."))
	d.Assert(tuitest.Not(tuitest.Matches(`(?m)^ {40,}- Also, what's 3\+3\?\s*$`)))
	require.Equal(t, 1, strings.Count(d.Frame(), "Also, what's 3+3?"), "FIFO promotion renders the accepted input once in transcript")
	select {
	case advanced := <-promoted:
		require.Equal(t, admitted, advanced, "promotion preserves accepted session/turn/position identity")
	case <-time.After(10 * time.Second):
		t.Fatal("canonical pending promotion was not observed")
	}
}

type queuedIdentity struct {
	sessionID, turnID string
	position          int
}

// queuedInputObserver records admission identity without replacing any dialog.
type queuedInputObserver struct {
	tea.Model

	accepted, promoted chan queuedIdentity
}

func (m *queuedInputObserver) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	inner := msg
	if routed, ok := inner.(messages.RoutedMsg); ok {
		inner = routed.Inner
	}
	if bridged, ok := inner.(messages.SessionRuntimeEventMsg); ok {
		inner = bridged.Event
	}
	switch event := inner.(type) {
	case *agentruntime.PendingUserMessageAcceptedEvent:
		if event.Message == "Also, what's 3+3?" {
			select {
			case m.accepted <- queuedIdentity{event.SessionID, event.TurnID, event.SessionPosition}:
			default:
			}
		}
	case *agentruntime.PendingUserMessagePromotedEvent:
		if event.Message == "Also, what's 3+3?" {
			select {
			case m.promoted <- queuedIdentity{event.SessionID, event.TurnID, event.SessionPosition}:
			default:
			}
		}
	}

	updated, cmd := m.Model.Update(msg)
	m.Model = updated
	return m, cmd
}

func (m *queuedInputObserver) SetProgram(p *tea.Program) {
	if owner, ok := m.Model.(interface{ SetProgram(p *tea.Program) }); ok {
		owner.SetProgram(p)
	}
}

func (m *queuedInputObserver) Shutdown() {
	if owner, ok := m.Model.(interface{ Shutdown() }); ok {
		owner.Shutdown()
	}
}

// Keep the manager's original settings instance: Save closes that exact model.
func openBehaviorSettings(d *tuitest.Driver) *tuitest.Driver {
	return d.Send(messages.OpenSettingsDialogMsg{}).
		WaitFor(tuitest.Contains("Appearance")).
		Send(tea.KeyPressMsg{Code: tea.KeyTab, Mod: tea.ModShift}).
		Press(tea.KeyRight).
		WaitFor(tuitest.Contains("While agent is working")).
		Enter()
}

func saveSettingsAndWait(d *tuitest.Driver) {
	d.Press(tea.KeyTab).Enter().
		WaitFor(tuitest.Contains("Settings saved")).
		WaitFor(tuitest.Absent("While agent is working")).
		WaitForStable(200 * time.Millisecond)
}

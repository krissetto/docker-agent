// Package tui_test contains end-to-end tests for the docker-agent terminal UI.
//
// These tests drive the real top-level TUI model through the tuitest harness
// (pkg/tui/tuitest) against a replaying VCR proxy, so a whole user journey —
// type a prompt, submit it, watch the agent stream its answer — runs offline
// and deterministically. They are the regression net for the finished
// product: a visual or behavioral change in a covered screen surfaces as a
// failed matcher or a golden diff.
package tui_test

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/tui"
	"github.com/docker/docker-agent/pkg/tui/core"
	"github.com/docker/docker-agent/pkg/tui/service"
	"github.com/docker/docker-agent/pkg/tui/tuitest"
	"github.com/docker/docker-agent/pkg/tui/types"
)

// TestChat_BasicMath types a question, submits it, and waits for the agent's
// streamed answer to appear in the transcript. The agent's response is
// replayed from testdata/cassettes/TestChat_BasicMath.yaml.
func TestChat_BasicMath(t *testing.T) {
	d := newTUI(t, "testdata/basic.yaml", 120, 40)

	d.Type("What's 2+2?").
		Enter().
		WaitFor(tuitest.Contains("What's 2+2?")). // the user's message echoes in the transcript
		WaitFor(tuitest.Contains("2 + 2 equals 4."))
}

// TestChat_PromptIsEditable proves typed input shows up in the editor before
// submission and moves into the transcript after sending — a basic but
// easy-to-regress piece of the finished product.
func TestChat_PromptIsEditable(t *testing.T) {
	d := newTUI(t, "testdata/basic.yaml", 120, 40)

	d.Type("What's 2+2?").
		WaitFor(tuitest.Contains("What's 2+2?"))

	// After submitting, the draft is sent and the agent's reply streams in.
	d.Enter().
		WaitFor(tuitest.Contains("2 + 2 equals 4."))
}

func TestChat_CopyAssistantMessageToClipboard(t *testing.T) {
	d := newTUI(t, "testdata/basic.yaml", 120, 40, tui.WithHideSidebar())

	d.Type("What's 2+2?").
		Enter().
		WaitFor(tuitest.Contains("2 + 2 equals 4."))

	d.MoveMouseToText("2 + 2 equals 4.").
		WaitFor(tuitest.Contains(types.MessageCopyLabel)).
		ClickText(types.MessageCopyLabel).
		WaitForClipboard("2 + 2 equals 4.").
		// The clicked label transiently reads "copied" instead of "⎘ copy".
		WaitFor(tuitest.Not(tuitest.Contains(types.MessageCopyLabel))).
		WaitFor(tuitest.Contains(types.CopiedFeedbackLabel))
}

// TestChat_DragSelectionCopiesAcrossRegions guards the mouse routing that
// keeps a text-selection drag alive outside the chat content region: the
// drag starts on the response text and releases near the bottom of the
// screen (over the editor/status area). The release must still finalize the
// selection and copy it.
func TestChat_DragSelectionCopiesAcrossRegions(t *testing.T) {
	d := newTUI(t, "testdata/basic.yaml", 120, 40, tui.WithHideSidebar())

	d.Type("What's 2+2?").
		Enter().
		WaitFor(tuitest.Contains("2 + 2 equals 4."))

	x, y := d.MustFindText("2 + 2 equals 4.")
	d.Drag(x, y, x+30, 37).
		WaitForClipboard("2 + 2 equals 4.")
}

// TestCommandPalette_Opens exercises a pure-UI interaction that needs no agent
// response: Ctrl+K opens the command palette overlay, Esc closes it. It runs
// against an empty cassette since no LLM call is made.
func TestCommandPalette_Opens(t *testing.T) {
	d := newTUI(t, "testdata/basic.yaml", 120, 40)

	// Wait for the UI to finish its initial render before interacting.
	d.WaitFor(tuitest.Not(tuitest.Contains("Loading")))

	// The palette shows a distinctive search placeholder when open.
	const placeholder = "Type to search commands"
	d.Assert(tuitest.Absent(placeholder))

	d.Press('k', tea.ModCtrl).
		WaitFor(tuitest.Contains(placeholder))

	// Esc closes the palette again.
	d.Press(tea.KeyEscape).
		WaitFor(tuitest.Absent(placeholder))
}

// TestGolden_Chat_BasicMath captures the first canonical completed frame whose
// currently visible presentation transitions have finished. The wrapper pins
// output only; the real model and its three-second notice expiry keep running.
func TestGolden_Chat_BasicMath(t *testing.T) {
	captured := make(chan tea.View, 1)
	expired := make(chan struct{}, 1)
	d := newTUIWithProxyOptionsWrapped(t, "testdata/basic.yaml", 120, 40, nil,
		func(model tea.Model) tea.Model {
			return &settledGoldenModel{inner: model, captured: captured, expired: expired}
		}, tui.WithHideSidebar(), tui.WithVersion("test"))

	d.Type("What's 2+2?").Enter()
	var snapshot tea.View
	select {
	case snapshot = <-captured:
	case <-time.After(10 * time.Second):
		t.Fatal("canonical completed notice never reached a settled presentation before test cancellation")
	}
	// Drain the event-loop barrier after captureModel has recorded the pinned
	// wrapper View, rather than reading a frame between Update and recordFrame.
	d.Send(settledGoldenBarrier{})
	require.Len(t, strings.Split(snapshot.Content, "\n"), 40)
	require.Equal(t, goldenCompletedTitle+" - docker agent", snapshot.WindowTitle, "canonical full title remains available without a pane header")
	d.Assert(tuitest.ContainsAll("2 + 2 equals 4.", goldenCompletedTabLabel, "root · 1 turn completed"))
	d.Assert(tuitest.Absent("Send to"))
	d.AssertGolden("chat_basic_math")
	// Pinning output must not suppress the underlying canonical expiry path.
	select {
	case <-expired:
	case <-time.After(10 * time.Second):
		t.Fatal("underlying summary expiry did not continue after golden capture")
	}
}

const (
	goldenCompletedTitle    = "Simple Math: Addition of 2 and 2"
	goldenCompletedTabLabel = "Simple Math: Ad…"
)

type settledGoldenBarrier struct{}

type settledGoldenModel struct {
	inner          tea.Model
	captured       chan<- tea.View
	expired        chan<- struct{}
	pinned         *tea.View
	expiryObserved bool
}

func (m *settledGoldenModel) SetProgram(program *tea.Program) {
	m.inner.(interface{ SetProgram(program *tea.Program) }).SetProgram(program)
}

func (m *settledGoldenModel) Shutdown()     { m.inner.(interface{ Shutdown() }).Shutdown() }
func (m *settledGoldenModel) Init() tea.Cmd { return m.inner.Init() }
func (m *settledGoldenModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if _, barrier := msg.(settledGoldenBarrier); barrier {
		return m, nil
	}
	inner, cmd := m.inner.Update(msg)
	m.inner = inner
	view := inner.View()
	plain := ansi.Strip(view.Content)
	settled := inner.(interface{ SettledPresentation() bool }).SettledPresentation()
	// The bounded tab no longer borrows a full-width pane heading. Verify
	// the full canonical title through existing dependency/state seams and
	// require the actual visible tab label separately; never widen the tab.
	application := core.Resolve[*app.App](inner)
	state := core.Resolve[*service.SessionState](inner)
	canonicalTitle := application.Session() != nil && application.Session().TitleSnapshot() == goldenCompletedTitle && state.SessionTitle() == goldenCompletedTitle
	if m.pinned == nil && settled && strings.Contains(plain, "2 + 2 equals 4.") &&
		!strings.Contains(plain, "Send to") && strings.Contains(plain, "root · 1 turn completed") &&
		canonicalTitle && view.WindowTitle == goldenCompletedTitle+" - docker agent" && strings.Contains(plain, goldenCompletedTabLabel) {
		snapshot := view
		m.pinned = &snapshot
		m.captured <- snapshot
	} else if m.pinned != nil && settled && !m.expiryObserved && !strings.Contains(plain, "root · 1 turn completed") {
		m.expiryObserved = true
		m.expired <- struct{}{}
	}
	return m, cmd
}

func (m *settledGoldenModel) View() tea.View {
	if m.pinned != nil {
		return *m.pinned
	}
	return m.inner.View()
}

package tui

import (
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/dialog"
	"github.com/docker/docker-agent/pkg/tui/messages"
)

type plusFeedbackFrame struct {
	tab    string
	active int32
	modal  bool
}

type plusFeedbackProgram struct {
	root                  *appModel
	mu                    sync.Mutex
	frames                []plusFeedbackFrame
	spawns, acceptedTicks int
	rendered              map[string]bool
	pressed               string
	pressedActive         int32
}

func (*plusFeedbackProgram) Init() tea.Cmd { return nil }
func (m *plusFeedbackProgram) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	before := m.root.ar.Now()
	_, cmd := m.root.Update(msg)
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := msg.(messages.SpawnSessionMsg); ok {
		m.spawns++
	}
	if _, ok := msg.(animation.TickMsg); ok && m.root.ar.Now() != before {
		m.acceptedTicks++
	}
	if click, ok := msg.(tea.MouseClickMsg); ok && click.Button == tea.MouseLeft && !m.root.dialogMgr.Open() && m.pressed == "" {
		m.pressed = m.root.tabBar.View()
		m.pressedActive = m.root.ar.ActiveCount()
	}
	m.frames = append(m.frames, plusFeedbackFrame{tab: m.root.tabBar.View(), active: m.root.ar.ActiveCount(), modal: m.root.dialogMgr.Open()})
	return m, cmd
}

func (m *plusFeedbackProgram) View() tea.View {
	view := m.root.View()
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rendered[m.root.tabBar.View()] = true
	return view
}

func (m *plusFeedbackProgram) snapshot() ([]plusFeedbackFrame, int, int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]plusFeedbackFrame(nil), m.frames...), m.spawns, m.acceptedTicks
}

func TestActualProgramPlusFeedbackFinishesUnderExistingPickerWithoutPointerInput(t *testing.T) {
	for _, mode := range []string{"stationary", "released-outside", "move-away", "closing"} {
		t.Run(mode, func(t *testing.T) {
			root, _, _ := wallClockRoot(t, 120, 40)
			normal := root.tabBar.View()
			plain := ansi.Strip(normal)
			index := strings.LastIndex(plain, "+")
			require.GreaterOrEqual(t, index, 0)
			x, y := ansi.StringWidth(plain[:index])+tabFrameOrigin(), root.contentHeight+1
			root.viewCacheValid = false
			model := &plusFeedbackProgram{root: root, rendered: make(map[string]bool)}
			writer := &cacheProgramWriter{}
			program := startTestProgram(t, root, model, tea.WithOutput(writer))
			require.Eventually(t, func() bool { frames, _, _ := model.snapshot(); return len(frames) > 0 }, time.Second, time.Millisecond)
			program.Send(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
			if mode == "move-away" {
				program.Send(tea.MouseMotionMsg{X: 0, Y: 0, Button: tea.MouseLeft})
			}
			if mode == "released-outside" {
				program.Send(tea.MouseReleaseMsg{X: 0, Y: 0, Button: tea.MouseLeft})
			}
			require.Eventually(t, func() bool { frames, spawns, _ := model.snapshot(); return spawns == 1 && frames[len(frames)-1].modal }, time.Second, time.Millisecond)
			if mode == "closing" {
				program.Send(dialog.CloseDialogMsg{})
				// Closing remains modal to new input, but the already-started feedback must finish.
				program.Send(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
			}
			// Poll the recorder only: no snapshot messages or pointer events can rescue the tick chain.
			require.Eventually(t, func() bool { frames, _, _ := model.snapshot(); return frames[len(frames)-1].active == 0 }, 2*time.Second, time.Millisecond)
			frames, spawns, ticks := model.snapshot()
			require.Equal(t, 1, spawns, "one accepted click opens the existing picker once")
			model.mu.Lock()
			pressed, pressedActive, rendered := model.pressed, model.pressedActive, len(model.rendered)
			model.mu.Unlock()
			require.Positive(t, pressedActive, "click synchronously registers feedback before its spawn command runs")
			require.NotEqual(t, normal, pressed)
			require.GreaterOrEqual(t, rendered, 3, "actual Bubble Tea View publishes distinct feedback states")
			require.Positive(t, ticks)
			target := normal
			require.Equal(t, target, frames[len(frames)-1].tab, "foreground picker occludes tab hover while finite click feedback completes")
			states := map[string]bool{}
			modalFrames := 0
			for _, frame := range frames {
				states[frame.tab] = true
				if frame.modal && frame.active > 0 {
					modalFrames++
				}
			}
			require.GreaterOrEqual(t, len(states), 4, "click feedback must render pressed and intermediate frames")
			require.GreaterOrEqual(t, modalFrames, 2, "feedback continues while the modal owns input")
			select {
			case <-time.After(80 * time.Millisecond):
			case <-t.Context().Done():
				t.Fatal("cancelled awaiting final terminal flush")
			}
			writes := len(writer.snapshot())
			_, _, settledTicks := model.snapshot()
			require.Never(t, func() bool {
				_, _, now := model.snapshot()
				return now != settledTicks || len(writer.snapshot()) != writes
			}, 80*time.Millisecond, time.Millisecond, "settled feedback and dialog must be idle")
		})
	}
}

func TestActualProgramPlusHoverEntryExitHasIntermediateFramesWithoutRescue(t *testing.T) {
	root, _, _ := wallClockRoot(t, 120, 40)
	root.editor.Blur()
	normal := root.tabBar.View()
	plain := ansi.Strip(normal)
	before, _, found := strings.Cut(plain, "+")
	require.True(t, found)
	x, y := ansi.StringWidth(before)+tabFrameOrigin(), root.contentHeight+1
	model := &plusFeedbackProgram{root: root, rendered: make(map[string]bool)}
	writer := &cacheProgramWriter{}
	program := startTestProgram(t, root, model, tea.WithOutput(writer))
	require.Eventually(t, func() bool { frames, _, _ := model.snapshot(); return len(frames) > 0 }, time.Second, time.Millisecond)
	program.Send(tea.MouseMotionMsg{X: x, Y: y})
	require.Eventually(t, func() bool {
		frames, _, ticks := model.snapshot()
		last := frames[len(frames)-1]
		return ticks > 0 && last.active == 0 && last.tab != normal
	}, time.Second, time.Millisecond)
	entered, spawns, _ := model.snapshot()
	require.Zero(t, spawns)
	states := map[string]bool{}
	for _, frame := range entered {
		states[frame.tab] = true
		require.Equal(t, ansi.Strip(normal), ansi.Strip(frame.tab), "hover changes color only")
	}
	require.GreaterOrEqual(t, len(states), 3, "entry includes intermediate colors")
	program.Send(tea.MouseMotionMsg{X: 0, Y: 0})
	require.Eventually(t, func() bool {
		frames, _, _ := model.snapshot()
		last := frames[len(frames)-1]
		return last.active == 0 && last.tab == normal
	}, time.Second, time.Millisecond)
	exited, _, ticks := model.snapshot()
	states = map[string]bool{}
	for _, frame := range exited[len(entered):] {
		states[frame.tab] = true
	}
	require.GreaterOrEqual(t, len(states), 3, "exit advances without a later pointer event")
	select {
	case <-time.After(80 * time.Millisecond):
	case <-t.Context().Done():
		t.Fatal("cancelled waiting terminalflush")
	}
	writes := len(writer.snapshot())
	require.Never(t, func() bool {
		_, _, after := model.snapshot()
		return after != ticks || len(writer.snapshot()) != writes
	}, 80*time.Millisecond, time.Millisecond, "settled hover is idle")
}

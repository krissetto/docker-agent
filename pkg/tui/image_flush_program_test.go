package tui

import (
	"bytes"
	tea "charm.land/bubbletea/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/docker/docker-agent/pkg/tui/core/layout"
	"github.com/docker/docker-agent/pkg/tui/dialog"
	tuiimage "github.com/docker/docker-agent/pkg/tui/image"
	"github.com/stretchr/testify/require"
	stdimage "image"
	"image/png"
	"strings"
	"sync"
	"testing"
	"time"
)

type graphicsProgramOutput struct {
	mu   sync.Mutex
	data bytes.Buffer
}

func (w *graphicsProgramOutput) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.data.Write(p)
}
func (w *graphicsProgramOutput) text() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.data.String()
}

type graphicsProgramFrame struct {
	active  int32
	markers bool
	updates int
}
type graphicsProgramModel struct {
	root    *appModel
	mu      sync.Mutex
	frame   graphicsProgramFrame
	updates int
}

func (m *graphicsProgramModel) Init() tea.Cmd { return nil }
func (m *graphicsProgramModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	m.updates++
	_, cmd := m.root.Update(msg)
	return m, cmd
}
func (m *graphicsProgramModel) View() tea.View {
	view := m.root.View()
	markers := false
	for _, layer := range m.root.dialogMgr.GetLayerInfos() {
		markers = markers || strings.Contains(layer.Content, "cagent-image;")
	}
	m.mu.Lock()
	m.frame = graphicsProgramFrame{active: m.root.ar.ActiveCount(), markers: markers, updates: m.updates}
	m.mu.Unlock()
	return view
}
func (m *graphicsProgramModel) snapshot() graphicsProgramFrame {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.frame
}

func TestImageFinalAnimationFrameFlushesWithoutFurtherInput(t *testing.T) {
	root, _, _ := wallClockRoot(t, 120, 40)
	root.editor.Blur()
	output := &graphicsProgramOutput{}
	writer := tuiimage.NewWriter(output)
	root.imageWriter = writer
	var data bytes.Buffer
	require.NoError(t, png.Encode(&data, stdimage.NewRGBA(stdimage.Rect(0, 0, 600, 400))))
	model := &graphicsProgramModel{root: root}
	program := startTestProgram(t, root, model, tea.WithOutput(writer), tea.WithFPS(120))
	program.Send(dialog.OpenDialogMsg{Model: dialog.NewImageAttachmentPreviewDialog(root.ar, "image.png", "image/png", data.Bytes(), true)})
	require.Eventually(t, func() bool { f := model.snapshot(); return f.active == 0 && f.markers }, 2*time.Second, time.Millisecond)
	require.Eventually(t, func() bool { return strings.Contains(output.text(), "a=p,i=") }, time.Second, time.Millisecond)
	// Same dimensions still start a fresh geometry transition: final markers may
	// return after the renderer has already painted identical title/border cells.
	for _, size := range [][2]int{{119, 40}, {120, 40}, {80, 30}, {120, 40}} {
		before := len(output.text())
		program.Send(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
		require.Eventually(t, func() bool { f := model.snapshot(); return f.active == 0 && f.markers }, 2*time.Second, time.Millisecond)
		require.Eventually(t, func() bool { return strings.Contains(output.text()[before:], "a=p,i=") }, time.Second, time.Millisecond, "settled graphics must place without another resize/key/forced View")
	}
	before := len(output.text())
	program.Send(uv.CellSizeEvent{Width: 9, Height: 23})
	require.Eventually(t, func() bool {
		f := model.snapshot()
		return f.active == 0 && f.markers && strings.Contains(output.text()[before:], "a=p,i=")
	}, 2*time.Second, time.Millisecond, "late cell metrics settle with a placement, no extra event")
	program.Send(tea.WindowSizeMsg{Width: 8, Height: 4})
	program.Send(tea.WindowSizeMsg{Width: 120, Height: 40})
	before = len(output.text())
	require.Eventually(t, func() bool {
		f := model.snapshot()
		return f.active == 0 && f.markers && strings.Contains(output.text()[before:], "a=p,i=")
	}, 2*time.Second, time.Millisecond, "tiny resize reversal recovers graphics")
	before = len(output.text())
	program.Send(tea.KeyPressMsg{Code: tea.KeyEscape})
	require.Eventually(t, func() bool {
		f := model.snapshot()
		return f.active == 0 && !f.markers && strings.Contains(output.text()[before:], "a=d,d=")
	}, 2*time.Second, time.Millisecond, "close retires graphics without another input")
}

type graphicsOnlyDialog struct {
	dialog.BaseDialog
	markers string
	visible bool
}

func (d *graphicsOnlyDialog) Init() tea.Cmd {
	return tea.Tick(150*time.Millisecond, func(time.Time) tea.Msg { return tea.KeyPressMsg{Code: 'x', Text: "x"} })
}
func (d *graphicsOnlyDialog) Update(msg tea.Msg) (layout.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		d.SetSize(msg.Width, msg.Height)
	case tea.KeyPressMsg:
		if msg.Code == 'x' {
			d.visible = true
		}
	}
	return d, nil
}
func (d *graphicsOnlyDialog) View() string {
	if d.visible {
		return "title\n     \n" + d.markers + "     "
	}
	return "title\n     \n     "
}
func (d *graphicsOnlyDialog) Position() (int, int)                  { return d.CenterDialog(d.View()) }
func (d *graphicsOnlyDialog) DisableDialogLifecycleAnimation() bool { return true }

func TestGraphicsOnlyScheduledFrameFlushesThroughActualProgram(t *testing.T) {
	root, _, _ := wallClockRoot(t, 120, 40)
	root.editor.Blur()
	output := &graphicsProgramOutput{}
	writer := tuiimage.NewWriter(output)
	root.imageWriter = writer
	var data bytes.Buffer
	require.NoError(t, png.Encode(&data, stdimage.NewRGBA(stdimage.Rect(0, 0, 8, 16))))
	preview, err := tuiimage.DecodePreview("one-cell", data.Bytes())
	require.NoError(t, err)
	defer preview.Close()
	fit, err := preview.Fit(1, 1, tuiimage.CellSize{})
	require.NoError(t, err)
	d := &graphicsOnlyDialog{markers: tuiimage.RenderNativePreviewMarkers(fit, tuiimage.CellSize{})[0]}
	model := &graphicsProgramModel{root: root}
	program := startTestProgram(t, root, model, tea.WithOutput(writer))
	program.Send(dialog.OpenDialogMsg{Model: d})
	require.Eventually(t, func() bool { return model.snapshot().markers }, time.Second, time.Millisecond)
	require.Eventually(t, func() bool { return strings.Contains(output.text(), "a=p,i=") }, time.Second, time.Millisecond, "graphics-only final state needs a write even though visible text is identical")
	time.Sleep(80 * time.Millisecond) // Allow the last renderer flush, not another model event.
	before := model.snapshot()
	wire := output.text()
	require.Never(t, func() bool { return model.snapshot() != before || output.text() != wire }, 100*time.Millisecond, time.Millisecond, "graphics flush settles without an idle wakeup loop")
}

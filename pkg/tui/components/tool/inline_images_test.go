package tool

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/core/layout"
	tuiimage "github.com/docker/docker-agent/pkg/tui/image"
)

type fakeShellInteraction struct {
	held       int
	wheelCalls int
}

func (m *fakeShellInteraction) Init() tea.Cmd                              { return nil }
func (m *fakeShellInteraction) Update(msg tea.Msg) (layout.Model, tea.Cmd) { return m, nil }
func (m *fakeShellInteraction) SetSize(int, int) tea.Cmd                   { return nil }
func (m *fakeShellInteraction) View() string                               { return "tool header\ntool body" }
func (m *fakeShellInteraction) WheelAt(line, col, delta int) bool {
	m.wheelCalls++
	return line == 1 && col == 2 && delta == 2
}
func (m *fakeShellInteraction) SetHoveredAt(line int, hovered bool) bool {
	if line == 0 {
		if hovered {
			m.held++
		} else {
			m.held--
		}
		return true
	}
	return false
}

func TestInlineImagesForwardShellHoverAndWheelOnlyWithinToolRows(t *testing.T) {
	ar := animation.NewRuntime()
	t.Cleanup(ar.Stop)
	child := &fakeShellInteraction{}
	tuiimage.SetRenderingEnabled(true)
	t.Cleanup(func() { tuiimage.SetRenderingEnabled(true) })
	image := tuiimage.Inline{Name: "x", PNGData: []byte("png"), Width: 1, Height: 40}
	view := withInlineImages(child, []tuiimage.Inline{image}, nil)
	wrapper := view.(*inlineImagesModel)
	wrapper.SetSize(20, 0)
	require.True(t, wrapper.SetHoveredAt(0, true))
	require.Equal(t, 1, child.held)
	markerLines := strings.Split(wrapper.View(), "\n")
	require.Greater(t, len(markerLines), 2, "the inline marker occupies rows after the tool content")
	require.False(t, wrapper.SetHoveredAt(2, true), "image marker rows don't hover the tool header")
	require.True(t, wrapper.WheelAt(1, 2, 2))
	require.False(t, wrapper.WheelAt(2, 2, 2), "image marker rows leave wheel routing to transcript")
	require.Equal(t, 1, child.wheelCalls)
}

func TestInlineImagesPreserveBodyScrollAcrossBothWrapperDirections(t *testing.T) {
	ar := animation.NewRuntime()
	t.Cleanup(ar.Stop)
	for _, tc := range []struct {
		name     string
		oldWrap  bool
		nextWrap bool
	}{
		{name: "wrapper-to-wrapper", oldWrap: true, nextWrap: true},
		{name: "wrapper-to-raw", oldWrap: true},
		{name: "raw-to-wrapper", nextWrap: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			oldShell := &fakeBodyScroll{offset: 4, bottom: true}
			nextShell := &fakeBodyScroll{}
			var previous, next any = oldShell, nextShell
			image := tuiimage.Inline{Name: "x", PNGData: []byte("png"), Width: 1, Height: 1}
			if tc.oldWrap {
				previous = withInlineImages(oldShell, []tuiimage.Inline{image}, nil)
			}
			if tc.nextWrap {
				next = withInlineImages(nextShell, []tuiimage.Inline{image}, nil)
			}
			transfer := next.(interface{ PreserveBodyScrollFrom(previous any) })
			transfer.PreserveBodyScrollFrom(previous)
			require.Equal(t, 4, nextShell.receivedOffset)
			require.True(t, nextShell.receivedBottom)
			require.True(t, nextShell.received)
		})
	}
}

type fakeBodyScroll struct {
	offset         int
	bottom         bool
	receivedOffset int
	receivedBottom bool
	received       bool
}

func (m *fakeBodyScroll) Init() tea.Cmd                              { return nil }
func (m *fakeBodyScroll) Update(msg tea.Msg) (layout.Model, tea.Cmd) { return m, nil }
func (m *fakeBodyScroll) SetSize(int, int) tea.Cmd                   { return nil }
func (m *fakeBodyScroll) View() string                               { return "body" }
func (m *fakeBodyScroll) BodyScrollState() (int, bool, bool)         { return m.offset, m.bottom, true }
func (m *fakeBodyScroll) PreserveBodyScrollFrom(previous any) {
	view, ok := previous.(interface{ BodyScrollState() (int, bool, bool) })
	if !ok {
		return
	}
	m.receivedOffset, m.receivedBottom, m.received = view.BodyScrollState()
}

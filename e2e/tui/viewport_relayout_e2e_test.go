package tui_test

import (
	"testing"

	"github.com/docker/docker-agent/pkg/tui"
	"github.com/docker/docker-agent/pkg/tui/tuitest"
)

// TestViewportRelayout_RealWheelResizeThenCopy drives the real TUI through a
// real wheel, a terminal resize, and a drag selection on the rewrapped reply.
// The copy must still yield the exact reply text after the layout changed.
func TestViewportRelayout_RealWheelResizeThenCopy(t *testing.T) {
	d := newTUI(t, "testdata/basic.yaml", 120, 40, tui.WithHideSidebar())

	d.Type("What's 2+2?").
		Enter().
		WaitFor(tuitest.Contains("2 + 2 equals 4."))

	d.Wheel(40, 10, 3).
		Resize(90, 40).
		WaitFor(tuitest.Contains("2 + 2 equals 4.")).
		Wheel(40, 10, -3).
		WaitFor(tuitest.Contains("2 + 2 equals 4."))

	x, y := d.MustFindText("2 + 2 equals 4.")
	d.Drag(x, y, x+20, y).
		WaitForClipboard("2 + 2 equals 4.")
}

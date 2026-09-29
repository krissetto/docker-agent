package tui

import (
	"image/color"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/docker/docker-agent/pkg/tui/rendering/retained"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

// paneColor preserves indexed colors as well as RGB values: converting indexed
// colors to RGB would silently change the emitted terminal bytes.
type paneColor struct {
	kind       uint8
	index      uint8
	r, g, b, a uint32
}

func snapshotPaneColor(c color.Color) paneColor {
	switch c := c.(type) {
	case nil, lipgloss.NoColor:
		return paneColor{}
	case ansi.BasicColor:
		return paneColor{kind: 1, index: uint8(c)}
	case ansi.IndexedColor:
		return paneColor{kind: 2, index: uint8(c)}
	default:
		r, g, b, a := c.RGBA()
		return paneColor{kind: 3, r: r, g: g, b: b, a: a}
	}
}

func (c paneColor) color() color.Color {
	switch c.kind {
	case 1:
		return ansi.BasicColor(c.index)
	case 2:
		return ansi.IndexedColor(c.index)
	case 3:
		return color.RGBA64{R: uint16(c.r), G: uint16(c.g), B: uint16(c.b), A: uint16(c.a)}
	default:
		return lipgloss.NoColor{}
	}
}

type paneTitleLayoutInput struct {
	identity, activity, title string
	width                     int
}

type paneTitlePaintInput struct {
	content                string
	foreground, background paneColor
	dimmed                 bool
	fade                   styles.FadeContext
}

type paneTitleEntry struct {
	layout retained.Slot[paneTitleLayoutInput]
	paint  retained.Slot[paneTitlePaintInput]
}

func drawPaneTitleLayout(in paneTitleLayoutInput) string {
	label := " " + in.identity + " " + in.activity + in.title
	if in.width <= 3 {
		label = "●"
	}
	return paneClipped(label, in.width, 1)
}

func drawPaneTitlePaint(in paneTitlePaintInput) string {
	surface := lipgloss.NewStyle().Foreground(in.foreground.color()).Background(in.background.color())
	heading := styles.RenderComposite(surface, in.content)
	if in.dimmed {
		heading = styles.FadeLineCtx(heading, inactivePaneContrast, &in.fade)
	}
	return heading
}

package editor

import (
	"charm.land/lipgloss/v2"

	"github.com/docker/docker-agent/pkg/tui/rendering/retained"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

// RenderDiagnostics counts actual widget materializations and presentation
// draws, including forced-fresh draws. Body counts full frame paints on every
// View; Composition counts retained inner textarea/history joins.
type RenderDiagnostics struct{ Textarea, Search, Body, Banner, Composition uint64 }

// RetainedRendering is an optional diagnostic capability, not a requirement on
// alternative Editor implementations. FreshView bypasses all body render caches.
type RetainedRendering interface {
	RenderDiagnostics() RenderDiagnostics
	FreshView() string
	FreshBannerView(width int) string
}

// editorInput is the complete, immutable input to inner editor composition.
// The full outer frame is materialized separately by the owner. Focus, selection
// and scrolling have already been materialized by the widget owner into Textarea;
// suggestions are overlaid after framing (see materializeFrame) and history state
// is represented by the optional History artifact.
// This is retained owner-materialized output, not an independent editable model
// or a second implementation of editing and wrapping geometry.
type editorInput struct {
	Textarea, History string
}

// drawEditor is the replaceable Charm paint adapter for plain string input and
// output. Lipgloss supplies ANSI-cell-aware row padding when joining history;
// only retained.Slot, not this paint adapter, is framework-independent.
func drawEditor(in editorInput) string {
	view := in.Textarea
	if in.History != "" {
		view = lipgloss.JoinVertical(lipgloss.Left, view, in.History)
	}
	return view
}

type editorRendering struct {
	composition retained.Slot[editorInput]
	body        uint64
}

// captureInput synchronizes presentation theme before
// publishing immutable artifacts. The retained renderer never sees a widget.
func (e *editor) captureInput(fresh bool) editorInput {
	if e.themeGeneration != styles.ThemeGeneration() {
		e.refreshTheme()
	}
	var view string
	if fresh {
		view = e.textarea.FreshView()
	} else {
		view = e.textarea.View()
	}
	history := ""
	if e.historySearch.active && e.height > 1 {
		if fresh {
			history = e.searchInput.FreshView()
		} else {
			history = e.searchInput.View()
		}
	}
	return editorInput{Textarea: view, History: history}
}

func (e *editor) suggestionShown() bool {
	return e.textarea.Focused() && e.hasSuggestion && e.suggestion != ""
}

// materializeFrame deliberately uses the complete authoritative Style rather
// than reconstructing selected properties. Styles can contain transforms,
// borders and properties without public getters. The resulting immutable ANSI
// artifact is what the root consumes; frame painting itself is not retained.
func (e *editor) materializeFrame(view string) string {
	e.rendering.body++
	frame := e.Frame()
	framed := renderInputFrame(frame, e.width, view)
	if !e.suggestionShown() {
		return framed
	}
	// Overlay after framing: RenderComposite re-applies the frame background after
	// every background SGR, which would erase the suggestion's cursor block.
	originX := frame.GetMarginLeft() + frame.GetBorderLeftSize() + frame.GetPaddingLeft()
	originY := frame.GetMarginTop() + frame.GetBorderTopSize() + frame.GetPaddingTop()
	return e.applySuggestionOverlay(framed, originX, originY)
}
func (e *editor) FreshView() string {
	return e.materializeFrame(e.rendering.composition.Fresh(e.captureInput(true), drawEditor))
}
func (e *editor) RenderDiagnostics() RenderDiagnostics {
	d := RenderDiagnostics{Textarea: e.textarea.Renders(), Body: e.rendering.body, Composition: e.rendering.composition.Renders()}
	if e.searchInput != nil {
		d.Search = e.searchInput.Renders()
	}
	if e.banner != nil {
		d.Banner = e.banner.renders
	}
	return d
}

func (e *editor) FreshBannerView(width int) string {
	if e.banner == nil {
		return ""
	}
	e.banner.SetSize(width)
	e.banner.reflow()
	return e.banner.view
}

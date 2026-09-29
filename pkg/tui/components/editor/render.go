package editor

import (
	"charm.land/lipgloss/v2"

	"github.com/docker/docker-agent/pkg/tui/rendering/retained"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

// RenderDiagnostics counts actual widget materializations and presentation
// draws, including forced-fresh draws. Textarea includes viewport normalization
// paints: Bubbles View itself mutates the owned viewport. Body counts full frame
// paints on every View; Composition counts retained inner textarea/history joins.
type RenderDiagnostics struct{ Textarea, Search, Body, Banner, Composition uint64 }

// RetainedRendering is an optional diagnostic capability, not a requirement on
// alternative Editor implementations. FreshView bypasses all body render caches.
type RetainedRendering interface {
	RenderDiagnostics() RenderDiagnostics
	FreshView() string
	FreshBannerView(width int) string
}

// editorInput is the complete, immutable input to inner editor composition.
// The full outer frame is materialized separately by the owner. Focus, selection,
// scrolling and suggestions have already been materialized by the widget owner
// into Textarea; history state is represented by the optional History artifact.
// This is retained owner-materialized output, not an independent editable model
// or a second implementation of Bubbles' private cursor/wrapping machinery.
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

type suggestionArtifact struct {
	generation, theme uint64
	suggestion        string
	valid             bool
	view              string
}

type editorRendering struct {
	composition retained.Slot[editorInput]
	body        uint64
	suggestion  suggestionArtifact
}

// captureInput synchronizes theme and normalizes widget side effects before
// publishing immutable artifacts. The retained renderer never sees a widget.
func (e *editor) captureInput(fresh bool) editorInput {
	if e.themeGeneration != styles.ThemeGeneration() {
		e.refreshTheme()
	}
	showSuggestion := e.textarea.Focused() && e.hasSuggestion && e.suggestion != ""
	if showSuggestion {
		e.fixViewportScroll()
	}
	var view string
	if fresh {
		view = e.textarea.FreshView()
	} else {
		view = e.textarea.View()
	}
	if showSuggestion {
		cached := &e.rendering.suggestion
		generation := e.textarea.Generation()
		if fresh || !cached.valid || cached.generation != generation || cached.theme != e.themeGeneration || cached.suggestion != e.suggestion {
			view = e.applySuggestionOverlay(view)
			if !fresh {
				*cached = suggestionArtifact{generation: e.textarea.Generation(), theme: e.themeGeneration, suggestion: e.suggestion, valid: true, view: view}
			}
		} else {
			view = cached.view
		}
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

// materializeFrame deliberately uses the complete authoritative Style rather
// than reconstructing selected properties. Styles can contain transforms,
// borders and properties without public getters. The resulting immutable ANSI
// artifact is what the root consumes; frame painting itself is not retained.
func (e *editor) materializeFrame(view string) string {
	e.rendering.body++
	frame := e.Frame()
	return styles.RenderComposite(frame.Width(e.width+frame.GetHorizontalPadding()+frame.GetHorizontalBorderSize()), view)
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

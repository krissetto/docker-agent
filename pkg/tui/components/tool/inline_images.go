package tool

import (
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/docker/docker-agent/pkg/app/lifecycle"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/core/layout"
	tuiimage "github.com/docker/docker-agent/pkg/tui/image"
	"github.com/docker/docker-agent/pkg/tui/service"
)

type inlineImagesModel struct {
	model        layout.Model
	images       []tuiimage.Inline
	sessionState service.SessionStateReader
	width        int
}

func withInlineImages(model layout.Model, images []tuiimage.Inline, sessionState service.SessionStateReader) layout.Model {
	if len(images) == 0 {
		return model
	}
	return &inlineImagesModel{model: model, images: images, sessionState: sessionState}
}

func (m *inlineImagesModel) Init() tea.Cmd {
	return m.model.Init()
}

func (m *inlineImagesModel) Update(msg tea.Msg) (layout.Model, tea.Cmd) {
	model, cmd := m.model.Update(msg)
	m.model = model
	return m, cmd
}

func (m *inlineImagesModel) SetSize(width, height int) tea.Cmd {
	m.width = width
	return m.model.SetSize(width, height)
}

func (m *inlineImagesModel) View() string {
	return m.render(m.model.View())
}

func (m *inlineImagesModel) ExpandedView() string {
	if expanded, ok := m.model.(interface{ ExpandedView() string }); ok {
		return m.render(expanded.ExpandedView())
	}
	return m.View()
}

func (m *inlineImagesModel) CollapsedView() string {
	if collapsed, ok := m.model.(layout.CollapsedViewer); ok {
		return collapsed.CollapsedView()
	}
	return m.model.View()
}

func (m *inlineImagesModel) StopAnimation() {
	animation.StopView(m.model)
}

// ResumeAnimation forwards only animation ownership, never image initialization.
func (m *inlineImagesModel) ResumeAnimation() tea.Cmd {
	if view, ok := m.model.(interface{ ResumeAnimation() tea.Cmd }); ok {
		return view.ResumeAnimation()
	}
	return nil
}

func (m *inlineImagesModel) render(content string) string {
	if m.sessionState != nil && m.sessionState.HideToolResults() {
		return content
	}
	parts := make([]string, 0, len(m.images)+1)
	if content = strings.TrimRight(content, "\n"); content != "" {
		parts = append(parts, content)
	}
	for _, img := range m.images {
		if lines := tuiimage.RenderMarkers(img, m.width); len(lines) > 0 {
			parts = append(parts, strings.Join(lines, "\n"))
		}
	}
	return strings.Join(parts, "\n")
}

func (m *inlineImagesModel) NeedsTick() bool {
	view, ok := m.model.(interface{ NeedsTick() bool })
	return ok && view.NeedsTick()
}

func (m *inlineImagesModel) PreserveBodyScrollFrom(previous any) {
	if view, ok := m.model.(interface{ PreserveBodyScrollFrom(previous any) }); ok {
		view.PreserveBodyScrollFrom(unwrapBodyScrollState(previous))
	}
}

func (m *inlineImagesModel) BodyScrollState() (int, bool, bool) {
	view, ok := m.model.(interface{ BodyScrollState() (int, bool, bool) })
	if !ok {
		return 0, false, false
	}
	return view.BodyScrollState()
}

func unwrapBodyScrollState(previous any) any {
	if view, ok := previous.(interface{ BodyScrollState() (int, bool, bool) }); ok {
		return bodyScrollStateSnapshot{source: view}
	}
	return previous
}

type bodyScrollStateSnapshot struct {
	source interface{ BodyScrollState() (int, bool, bool) }
}

func (s bodyScrollStateSnapshot) BodyScrollState() (int, bool, bool) {
	return s.source.BodyScrollState()
}

func (m *inlineImagesModel) IsExpanded() bool {
	view, ok := m.model.(interface{ IsExpanded() bool })
	return ok && view.IsExpanded()
}

func (m *inlineImagesModel) SetExpanded(expanded bool) {
	if view, ok := m.model.(interface{ SetExpanded(expanded bool) }); ok {
		view.SetExpanded(expanded)
	}
}

func (m *inlineImagesModel) PreserveExpansionOnParentCollapse() bool {
	view, ok := m.model.(interface{ PreserveExpansionOnParentCollapse() bool })
	return ok && view.PreserveExpansionOnParentCollapse()
}

func (m *inlineImagesModel) IsToggleLine(line int) bool {
	view, ok := m.model.(interface{ IsToggleLine(line int) bool })
	return ok && view.IsToggleLine(line)
}

func (m *inlineImagesModel) IsToggleAt(line, col int) bool {
	view, ok := m.model.(interface{ IsToggleAt(line, col int) bool })
	return ok && view.IsToggleAt(line, col)
}

func (m *inlineImagesModel) WheelAt(line, col, delta int) bool {
	// Appended image rows are outside the wrapped tool's coordinate space and
	// must leave wheel input available to the transcript.
	if line >= lineCount(m.model.View()) {
		return false
	}
	view, ok := m.model.(interface {
		WheelAt(line, col, delta int) bool
	})
	return ok && view.WheelAt(line, col, delta)
}

func (m *inlineImagesModel) SetHoveredAt(line int, hovered bool) bool {
	if line >= lineCount(m.model.View()) {
		return false
	}
	view, ok := m.model.(interface {
		SetHoveredAt(line int, hovered bool) bool
	})
	return ok && view.SetHoveredAt(line, hovered)
}

func lineCount(content string) int {
	if content == "" {
		return 0
	}
	return strings.Count(strings.TrimSuffix(content, "\n"), "\n") + 1
}

func (m *inlineImagesModel) Toggle() {
	if view, ok := m.model.(interface{ Toggle() }); ok {
		view.Toggle()
	}
}

func (m *inlineImagesModel) InputReferenceForLine(line int) (lifecycle.InputReference, bool) {
	if view, ok := m.model.(interface {
		InputReferenceForLine(line int) (lifecycle.InputReference, bool)
	}); ok {
		return view.InputReferenceForLine(line)
	}
	return lifecycle.InputReference{}, false
}

func (m *inlineImagesModel) InputReferenceOnLine(line int) bool {
	if view, ok := m.model.(interface{ InputReferenceOnLine(line int) bool }); ok {
		return view.InputReferenceOnLine(line)
	}
	return true
}

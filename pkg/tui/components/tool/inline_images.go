package tool

import (
	"strings"

	tea "charm.land/bubbletea/v2"

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

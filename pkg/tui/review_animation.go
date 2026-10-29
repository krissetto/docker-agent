package tui

import (
	"github.com/docker/docker-agent/pkg/tui/animation"
	view "github.com/docker/docker-agent/pkg/tui/components/review"
)

func (m *appModel) syncReviewAnimations() {
	if len(m.gitReviews) == 0 && len(m.reviewAnimations) == 0 {
		return
	}
	visible := make(map[*view.Model]struct{})
	if !m.leanMode && !m.tickPaused && m.err == nil && !m.dialogMgr.Open() {
		if surface := m.reviewSurface(); surface != nil {
			visible[surface] = struct{}{}
		}
		for id, workspace := range m.gitReviews {
			if workspace.open && workspace.inspecting && m.paneVisible(id) {
				if model := workspace.models[workspace.mode]; model != nil {
					visible[model] = struct{}{}
				}
			}
		}
	}
	for model := range m.reviewAnimations {
		if _, ok := visible[model]; !ok {
			model.StopAnimation()
		}
	}
	for model := range visible {
		if _, exists := m.reviewAnimations[model]; exists {
			continue
		}
		model.SetAnimationRuntime(m.ar)
		model.SetScrollbarActive(true)
	}
	m.reviewAnimations = visible
}

func (m *appModel) tickReviewAnimation(tick animation.TickMsg) {
	model := m.reviewSurface()
	if model == nil {
		return
	}
	if model.UpdateAnimation(tick) {
		tick.MarkDirty()
		m.viewCacheValid = false
	}
}

func (m *appModel) stopReviewAnimations() {
	for model := range m.reviewAnimations {
		model.StopAnimation()
	}
	m.reviewAnimations = nil
}

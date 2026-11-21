package messages

import (
	"slices"

	"github.com/docker/docker-agent/pkg/tui/types"
)

// Work moves as a display slot; source indices and all source-owned state stay put.
func (m *model) syncWorkOrder() {
	order := make([]int, len(m.views))
	for i := range order {
		order[i] = i
	}
	if m.workEnabled {
		for _, exchange := range m.workProjection.exchanges {
			if len(exchange.work) == 0 {
				continue
			}
			anchor := -1
			for _, source := range exchange.work {
				if source >= m.historyStart {
					anchor = source
					break
				}
			}
			if anchor < 0 {
				continue
			}
			first := anchor
			for _, source := range exchange.response {
				if source >= m.historyStart && m.messages[source].Type == types.MessageTypeAssistant {
					first = min(first, source)
				}
			}
			if first < anchor {
				copy(order[first+1:anchor+1], order[first:anchor])
				order[first] = anchor
			}
		}
	}
	identity := true
	for i, source := range order {
		if i != source {
			identity = false
			break
		}
	}
	if identity {
		order = nil
	}
	if !slices.Equal(m.geom.order, order) {
		m.markFacade()
	}
	m.geom.setOrder(order)
}

func (m *model) nextDisplaySlot(source int) int {
	if len(m.geom.positions) == len(m.views) && source >= 0 && source < len(m.views) {
		position := m.geom.positions[source] + 1
		if position < len(m.views) {
			return m.geom.order[position]
		}
		return len(m.views)
	}
	if source+1 < max(len(m.views), len(m.messages)) {
		return source + 1
	}
	return max(len(m.views), len(m.messages))
}

func (m *model) previousDisplaySlot(source int) int {
	if len(m.geom.order) == 0 {
		return source - 1
	}
	if source == len(m.views) {
		return m.geom.order[len(m.views)-1]
	}
	position := m.geom.positions[source] - 1
	if position < 0 {
		return -1
	}
	return m.geom.order[position]
}

func (x *layoutIndex) setOrder(order []int) {
	if slices.Equal(x.order, order) {
		return
	}
	x.order = order
	x.positions = make([]int, len(order))
	for position, source := range order {
		x.positions[source] = position
	}
}

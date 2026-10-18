package sidebar

import (
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/subagent"
)

func (m *model) seedSessionCosts(sess *session.Session, parent string) {
	if sess == nil {
		return
	}
	if _, seen := m.sessionOwnCost[sess.ID]; seen {
		return
	}
	m.sessionOwnCost[sess.ID] = sess.OwnCost()
	m.costParents[sess.ID] = parent
	m.embeddedCosts[sess.ID] = sess.EmbeddedSubSessionCost()
	for _, item := range sess.ItemsSnapshot() {
		if item.SubSession != nil {
			m.seedSessionCosts(item.SubSession, sess.ID)
		}
	}
}

// SubtreeCost unions own-cost snapshots by canonical session identity. Tree
// metrics include retained descendants even when they never stream into this view.
func (m *model) SubtreeCost() float64 {
	costs := make(map[string]float64)
	if m.delegationRoot != nil {
		costs[m.rootSessionID] = m.delegationRoot.Cost
	}
	var walk func([]subagent.NodeSnapshot)
	walk = func(nodes []subagent.NodeSnapshot) {
		for _, item := range nodes {
			id := item.Node.SessionID
			if id == "" {
				id = "node:" + string(item.Node.ID)
			}
			costs[id] = max(costs[id], item.Node.Cost)
			walk(item.Children)
		}
	}
	walk(m.subagentNodes)
	inScope := func(id string) bool {
		// Legacy forwarding already scopes events to this view; without a
		// canonical tree there is no stronger membership witness.
		if _, found := costs[id]; found || id == m.rootSessionID || m.rootSessionID == "" || m.delegationRoot == nil {
			return true
		}
		seen := make(map[string]bool)
		for parent := m.costParents[id]; parent != "" && !seen[parent]; parent = m.costParents[parent] {
			if parent == m.rootSessionID {
				return true
			}
			seen[parent] = true
		}
		return false
	}
	for id, cost := range m.sessionOwnCost {
		if inScope(id) {
			costs[id] = max(costs[id], cost)
		}
	}
	for id, usage := range m.sessionUsage {
		if !inScope(id) {
			continue
		}
		if _, ownKnown := m.sessionOwnCost[id]; ownKnown {
			continue
		}
		// Without own-cost metadata, prefer canonical metrics over an aggregate.
		if _, canonical := costs[id]; !canonical {
			costs[id] = usage.Cost
		}
	}
	var total float64
	for _, cost := range costs {
		total += cost
	}
	return total
}

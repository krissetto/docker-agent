package leantui

import (
	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/leantui/ui"
	"github.com/docker/docker-agent/pkg/runtime"
)

// newUsageTrackerFor binds the footer root to the view's attached session.
func newUsageTrackerFor(application *app.App) *ui.UsageTracker {
	tracker := ui.NewUsageTracker()
	if application != nil && application.Session() != nil {
		tracker.BindRoot(application.Session().ID)
	}
	return tracker
}

// trackStreamStarted records a newly-started stream and refreshes the footer.
func (m *model) trackStreamStarted(sessionID string) {
	m.usage.StreamStarted(sessionID)
	m.applyUsageSnapshot()
}

// trackStreamStopped records a finished stream and refreshes the footer.
func (m *model) trackStreamStopped() {
	m.usage.StreamStopped()
	m.applyUsageSnapshot()
}

func (m *model) setTokenUsage(sessionID string, usage *runtime.Usage) {
	if usage == nil {
		return
	}

	snapshot := ui.UsageSnapshot{
		ContextLength:       usage.ContextLength,
		ContextLimit:        usage.ContextLimit,
		Tokens:              usage.InputTokens + usage.OutputTokens,
		Cost:                usage.Cost,
		CompactionThreshold: usage.CompactionThreshold,
	}
	if sessionID == "" {
		// Once session-scoped usage exists, it is authoritative for the chat
		// footer. Empty-session usage comes from side work such as RAG indexing.
		if m.usage.Empty() {
			m.applyStatusUsage(snapshot, usage.Cost, true)
		}
		return
	}
	m.usage.Record(sessionID, snapshot)
	m.applyUsageSnapshot()
}

// applyUsageSnapshot pushes the tracker's derived footer usage onto the status
// line: the active session's context window plus the run's total cost.
func (m *model) applyUsageSnapshot() {
	if m.usage.Empty() {
		return
	}

	totalCost := m.usage.TotalCost()
	if usage, ok := m.usage.Active(); ok {
		m.applyStatusUsage(usage, totalCost, true)
		return
	}

	m.status.Cost = totalCost
	m.status.CostKnown = true
}

func (m *model) applyStatusUsage(usage ui.UsageSnapshot, cost float64, costKnown bool) {
	m.status.ContextLength = usage.ContextLength
	m.status.ContextLimit = usage.ContextLimit
	m.status.CompactionThreshold = usage.CompactionThreshold
	m.status.Tokens = usage.Tokens
	m.status.Cost = cost
	m.status.CostKnown = costKnown
}

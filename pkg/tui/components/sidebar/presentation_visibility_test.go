package sidebar

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/service"
)

func TestPresentationVisibilityResumesOnlyLiveSidebarLeases(t *testing.T) {
	ar := animation.NewRuntime()
	t.Cleanup(ar.Stop)
	m := New(ar, t.Context(), &service.SessionState{}).(*model)
	m.SetSize(45, 30)
	m.CancelPresentation()
	m.SetTitleRegenerating(true)
	m.SetAgentSwitching(true, "root", "worker")
	require.True(t, m.spinnerActive)
	require.True(t, m.transferAnimation.IsActive())
	require.Equal(t, int32(2), ar.ActiveCount())

	m.SetPresentationActive(false)
	require.Zero(t, ar.ActiveCount())
	m.Update(&runtime.RAGIndexingStartedEvent{RAGName: "docs", StrategyName: "local"})
	m.Update(&runtime.ToolsetInfoEvent{Loading: true})
	m.SetAgentSwitching(true, "worker", "nested")
	require.Zero(t, ar.ActiveCount(), "hidden ingestion cannot keep spinners or rail running")
	require.Len(t, m.ragIndexing, 1, "ingestion still updates canonical presentation state")

	m.SetPresentationActive(true)
	require.True(t, m.spinnerActive)
	require.True(t, m.transferAnimation.IsActive())
	require.Equal(t, int32(3), ar.ActiveCount(), "main, transfer, and RAG leases resume")
	require.Nil(t, m.SetPresentationActive(true))
	require.Equal(t, int32(3), ar.ActiveCount(), "reactivation is idempotent")

	m.SetPresentationActive(false)
	for _, hop := range append([]agentTransfer(nil), m.agentTransfers...) {
		m.Update(transferTimerMsg{gen: hop.gen, kind: transferTimerMax})
	}
	m.Update(messages.StreamCancelledMsg{})
	require.Zero(t, ar.ActiveCount())
	m.SetPresentationActive(true)
	require.Zero(t, ar.ActiveCount(), "settled hidden work must not resurrect any animation")
}

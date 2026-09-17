package leantui

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/leantui/ui"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tui/components/messagebar"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/service/tuistate"
)

func TestViewerBackstackRestoresDraftAndRoutesHiddenEvents(t *testing.T) {
	m, originalHandle := sessionModel(t)
	original := m.app
	m.screen.Editor.SetText("original draft")
	m.draftAttachments = []messages.Attachment{{Name: "original.txt", FilePath: "/fixture/original.txt"}}
	m.viewers = &viewerHost{views: make(map[*app.App]*model)}
	target, targetHandle := sessionModel(t)
	target.screen.Editor.SetText("child draft")
	target.draftAttachments = []messages.Attachment{{Name: "child.txt", FilePath: "/fixture/child.txt"}}
	m.focusViewer(target, true)
	assert.Equal(t, "child draft", m.screen.Editor.Text())
	m.routeViewerEvent(t.Context(), viewerEvent{origin: original, event: runtime.AgentChoice("agent", originalHandle.id, "original late answer")})
	assert.NotContains(t, strings.Join(m.screen.Transcript.Lines(80, 0, false, nil, nil), "\n"), "original late answer")
	m.handleViewerCommand(t.Context(), "back", "")
	assert.Same(t, original, m.app)
	assert.Equal(t, "original draft", m.screen.Editor.Text())
	require.Len(t, m.draftAttachments, 1)
	assert.Equal(t, "original.txt", m.draftAttachments[0].Name)
	assert.Contains(t, strings.Join(m.screen.Transcript.Lines(80, 0, false, nil, nil), "\n"), "original late answer")
	assert.Zero(t, originalHandle.stops)
	assert.Zero(t, targetHandle.stops)
}

func TestViewerLateAcceptedInputRetainsOrigin(t *testing.T) {
	m, originalHandle := sessionModel(t)
	original := m.app
	m.viewers = &viewerHost{views: make(map[*app.App]*model)}
	target, _ := sessionModel(t)
	m.focusViewer(target, true)
	m.routeViewerEvent(t.Context(), viewerEvent{origin: original, event: &runtime.PendingUserMessageAcceptedEvent{
		SessionID: originalHandle.id, TurnID: "accepted", Message: "original queued", InputOrigin: session.InputOriginUser,
	}})
	assert.Empty(t, m.pendingUsers)
	m.handleViewerCommand(t.Context(), "back", "")
	require.Len(t, m.pendingUsers, 1)
	assert.Equal(t, "accepted", m.pendingUsers[0].TurnID)
}

func TestViewerUnknownOriginIgnored(t *testing.T) {
	m, _ := sessionModel(t)
	m.viewers = &viewerHost{views: make(map[*app.App]*model)}
	before := m.screen.Transcript.BlockCount()
	m.routeViewerEvent(t.Context(), viewerEvent{origin: &app.App{}, event: runtime.Warning("foreign", "")})
	assert.Equal(t, before, m.screen.Transcript.BlockCount())
}

func TestViewerShutdownDetachesWithoutCancelAndCleansOwnedOnce(t *testing.T) {
	m, handle := sessionModel(t)
	ctx, cancel := context.WithCancel(t.Context())
	cleanups := 0
	host := &viewerHost{cancel: []context.CancelFunc{cancel}, cleanup: []func(){func() { cleanups++ }}}
	m.viewers = host
	host.close()
	host.close()
	require.ErrorIs(t, ctx.Err(), context.Canceled)
	assert.Equal(t, 1, cleanups)
	assert.Zero(t, handle.stops)
}

func TestUnsupportedSubagentRuntimeIsExplicit(t *testing.T) {
	m, _ := sessionModel(t)
	m.handleViewerCommand(t.Context(), "subagent-attach", "abcde")
	assert.Contains(t, strings.Join(m.screen.Transcript.Lines(80, 0, false, nil, nil), "\n"), "does not expose live subagent attachment")
}

func TestViewerFocusKeepsTerminalGeometry(t *testing.T) {
	m, _ := sessionModel(t)
	m.width, m.height = 100, 40
	m.viewers = &viewerHost{views: make(map[*app.App]*model)}
	target := *m
	target.app = &app.App{}
	target.width, target.height = 10, 5
	target.screen = ui.NewScreen("", "", "")
	m.focusViewer(&target, true)
	assert.Equal(t, 100, m.width)
	assert.Equal(t, 40, m.height)
}

func TestMajorEventsCountCanonicalFactsNotReplayOrFocus(t *testing.T) {
	m, handle := sessionModel(t)
	m.majorEvents = &messagebar.Aggregator{}
	m.handleEvent(t.Context(), app.SessionEventMsg{OriginSessionID: handle.id, Sequence: 1, Seed: true, Event: &runtime.TurnSettledEvent{SessionID: handle.id, TurnID: "old", Outcome: runtime.TurnCompleted}})
	_, visible := m.majorEvents.Current(handle.id, time.Now())
	assert.False(t, visible)
	m.handleEvent(t.Context(), app.SessionEventMsg{OriginSessionID: handle.id, Sequence: 2, Event: &runtime.TurnSettledEvent{SessionID: handle.id, TurnID: "turn", Outcome: runtime.TurnCompleted}})
	first, visible := m.majorEvents.Current(handle.id, time.Now())
	require.True(t, visible)
	assert.Equal(t, 1, first.CompletedTurns)
	m.handleEvent(t.Context(), app.SessionEventMsg{OriginSessionID: handle.id, Sequence: 2, Event: &runtime.TurnSettledEvent{SessionID: handle.id, TurnID: "turn", Outcome: runtime.TurnCompleted}})
	m.handleEvent(t.Context(), app.SessionEventMsg{OriginSessionID: handle.id, Sequence: 3, Event: &runtime.TurnSettledEvent{SessionID: handle.id, TurnID: "cancelled", Outcome: runtime.TurnCanceled}})
	current, _ := m.majorEvents.Current(handle.id, time.Now())
	assert.Equal(t, first.Generation, current.Generation)
	assert.Equal(t, first.Deadline, current.Deadline)
	before := m.screen.Transcript.BlockCount()
	m.buildLines()
	assert.Contains(t, m.screen.TransientNotice, "1 turn completed")
	assert.Equal(t, before, m.screen.Transcript.BlockCount())
	m.interruptPending = true
	m.buildLines()
	assert.Empty(t, m.screen.TransientNotice)
	_, visible = m.majorEvents.Current(handle.id, first.Deadline)
	assert.False(t, visible)
}

type memoryTabStore struct {
	entries []tuistate.TabEntry
	active  string
	clears  int
}

func (s *memoryTabStore) GetTabs(context.Context) ([]tuistate.TabEntry, string, error) {
	return s.entries, s.active, nil
}

func (s *memoryTabStore) AddTab(_ context.Context, id, dir string) error {
	s.entries = append(s.entries, tuistate.TabEntry{SessionID: id, WorkingDir: dir})
	return nil
}
func (s *memoryTabStore) RemoveTab(_ context.Context, id string) error    { return nil }
func (s *memoryTabStore) SetActiveTab(_ context.Context, id string) error { s.active = id; return nil }

func (s *memoryTabStore) ClearTabs(context.Context) error                          { s.entries = nil; s.clears++; return nil }
func (s *memoryTabStore) ReplaceTab(context.Context, string, string, string) error { return nil }

func TestRestoreMetadataHydratesOnlyActiveAndDeduplicates(t *testing.T) {
	m, _ := sessionModel(t)
	active, _ := sessionModel(t)
	store := &memoryTabStore{entries: []tuistate.TabEntry{
		{SessionID: "cold", WorkingDir: "/fixture/cold"},
		{SessionID: active.app.Session().ID, WorkingDir: "/fixture/active"},
		{SessionID: "cold", WorkingDir: "/fixture/duplicate"},
	}, active: active.app.Session().ID}
	var restored []string
	cleanups := 0
	m.restoreViewerMetadata(t.Context(), Config{TabStore: store, RestoreTabs: true, RestoreSession: func(_ context.Context, id, dir string) (*app.App, func(), error) {
		restored = append(restored, id)
		return active.app, func() { cleanups++ }, nil
	}})
	assert.Equal(t, []string{active.app.Session().ID}, restored)
	assert.Same(t, active.app, m.app)
	require.Len(t, m.restoredEntries, 2)
	assert.Equal(t, "cold", m.restoredEntries[0].SessionID)
	assert.Equal(t, 0, cleanups)
	host := &viewerHost{cleanup: m.restoredCleanup}
	host.close()
	host.close()
	assert.Equal(t, 1, cleanups)
}

func TestRestoreFailureKeepsInitialAndDoesNotHydrateOtherRows(t *testing.T) {
	m, _ := sessionModel(t)
	original := m.app
	store := &memoryTabStore{entries: []tuistate.TabEntry{{SessionID: "missing"}, {SessionID: "cold"}}, active: "missing"}
	calls := 0
	m.restoreViewerMetadata(t.Context(), Config{TabStore: store, RestoreTabs: true, RestoreSession: func(context.Context, string, string) (*app.App, func(), error) {
		calls++
		return nil, nil, errors.New("missing saved session")
	}})
	assert.Same(t, original, m.app)
	assert.Equal(t, 1, calls)
	assert.Equal(t, original.Session().ID, store.active)
	assert.Contains(t, strings.Join(m.screen.Transcript.Lines(100, 0, false, nil, nil), "\n"), "missing saved session")
}

func TestRestoreDisabledClearsSavedTabsWithoutHydration(t *testing.T) {
	m, _ := sessionModel(t)
	store := &memoryTabStore{entries: []tuistate.TabEntry{{SessionID: "old"}}, active: "old"}
	m.restoreViewerMetadata(t.Context(), Config{TabStore: store, RestoreTabs: false, RestoreSession: func(context.Context, string, string) (*app.App, func(), error) {
		t.Fatal("restore callback must not run when disabled")
		return nil, nil, nil
	}})
	assert.Equal(t, 1, store.clears)
	require.Len(t, store.entries, 1)
	assert.Equal(t, m.app.Session().ID, store.entries[0].SessionID)
}

type fixturePreparedHostedView struct {
	info      runtime.PreparedSessionViewInfo
	committed runtime.CommittedSessionView
	commitErr error
	aborts    int
	builds    int
}

func (v *fixturePreparedHostedView) Info() runtime.PreparedSessionViewInfo { return v.info }
func (v *fixturePreparedHostedView) Commit(context.Context) (runtime.CommittedSessionView, error) {
	return v.committed, v.commitErr
}
func (v *fixturePreparedHostedView) Abort() { v.aborts++ }
func (v *fixturePreparedHostedView) NewApp(context.Context, runtime.CommittedSessionView) (*app.App, error) {
	v.builds++
	return nil, errors.New("fixture construction denied")
}

func TestHostedViewResultDoesNotRetargetAfterFocusChange(t *testing.T) {
	m, handle := sessionModel(t)
	original := m.app
	m.viewers = &viewerHost{views: make(map[*app.App]*model)}
	prepared := &fixturePreparedHostedView{committed: runtime.CommittedSessionView{SessionHandle: handle}}
	result := acquiredSessionView{origin: original, identity: original.CurrentSessionEventIdentity(), generation: m.viewAcquireGeneration, prepared: prepared, committed: prepared.committed}
	target, _ := sessionModel(t)
	m.focusViewer(target, true)
	m.routeViewerEvent(t.Context(), result)
	assert.Same(t, target.app, m.app)
	assert.Equal(t, 1, prepared.aborts)
	assert.Zero(t, prepared.builds, "stale focus must be rejected before App construction")
	assert.Empty(t, m.viewers.cleanup, "borrowed host result cannot acquire runtime cleanup")
}

func TestHostedViewConstructionFailureRetainsCurrentDraft(t *testing.T) {
	m, handle := sessionModel(t)
	m.viewers = &viewerHost{ctx: t.Context, views: make(map[*app.App]*model)}
	m.screen.Editor.SetText("original draft")
	prepared := &fixturePreparedHostedView{committed: runtime.CommittedSessionView{SessionHandle: handle}}
	m.routeViewerEvent(t.Context(), acquiredSessionView{origin: m.app, identity: m.app.CurrentSessionEventIdentity(), generation: m.viewAcquireGeneration, prepared: prepared, committed: prepared.committed})
	assert.Equal(t, 1, prepared.builds)
	assert.Equal(t, 1, prepared.aborts)
	assert.Equal(t, "original draft", m.screen.Editor.Text())
	assert.Empty(t, m.viewers.cleanup)
	assert.Zero(t, handle.stops)
}

func TestHostedViewResultAfterShutdownOnlyAbortsPreparation(t *testing.T) {
	m, handle := sessionModel(t)
	m.viewers = &viewerHost{views: make(map[*app.App]*model)}
	prepared := &fixturePreparedHostedView{committed: runtime.CommittedSessionView{SessionHandle: handle}}
	result := acquiredSessionView{origin: m.app, identity: m.app.CurrentSessionEventIdentity(), generation: m.viewAcquireGeneration, prepared: prepared, committed: prepared.committed}
	m.viewers.close()
	m.routeViewerEvent(t.Context(), result)
	assert.Equal(t, 1, prepared.aborts)
	assert.Zero(t, prepared.builds)
	assert.Zero(t, handle.stops)
}

func TestConfirmationDraftBelongsToExactInteraction(t *testing.T) {
	for _, clear := range []string{"resolution", "replacement", "projection", "snapshot"} {
		t.Run(clear, func(t *testing.T) {
			m, handle := sessionModel(t)
			m.handleEvent(t.Context(), &runtime.ToolCallConfirmationEvent{SessionID: handle.id, RequestID: "old"})
			m.handleConfirmKey(t.Context(), ui.Key{Typ: ui.KeyRune, Runes: []rune("r")})
			m.handleConfirmKey(t.Context(), ui.Key{Typ: ui.KeyRune, Runes: []rune("old reason")})
			old := m.screen.Confirm
			require.True(t, old.Rejecting)
			require.Equal(t, "old reason", old.RejectReason)
			m.handleEvent(t.Context(), &runtime.ToolCallConfirmationEvent{SessionID: handle.id, RequestID: "old"})
			require.Same(t, old, m.screen.Confirm, "replayed identity preserves its draft")
			switch clear {
			case "resolution":
				m.handleEvent(t.Context(), &runtime.InteractionResolvedEvent{SessionID: "other", InteractionID: "old"})
				require.Same(t, old, m.screen.Confirm, "foreign resolution cannot clear the draft")
				m.handleEvent(t.Context(), &runtime.InteractionResolvedEvent{SessionID: handle.id, InteractionID: "old"})
			case "projection":
				event := m.app.CurrentSessionEventIdentity()
				event.Event = &runtime.InteractionResolvedEvent{SessionID: handle.id, InteractionID: "old"}
				event.Projection = &app.PresentationState{}
				m.handleEvent(t.Context(), event)
			case "snapshot":
				m.handleEvent(t.Context(), &app.SessionResetEvent{Snapshot: runtime.SessionSnapshot{Session: m.app.Session()}})
			}
			if clear != "replacement" {
				require.Nil(t, m.screen.Confirm)
			}
			m.handleEvent(t.Context(), &runtime.ToolCallConfirmationEvent{SessionID: handle.id, RequestID: "new"})
			require.False(t, m.screen.Confirm.Rejecting)
			require.Empty(t, m.screen.Confirm.RejectReason)
			m.handleConfirmKey(t.Context(), ui.Key{Typ: ui.KeyEnter})
			require.Empty(t, handle.responses, "Enter cannot reject the new interaction with an obsolete draft")
			m.handleConfirmKey(t.Context(), ui.Key{Typ: ui.KeyRune, Runes: []rune("y")})
			require.Len(t, handle.responses, 1)
			assert.Equal(t, "new", handle.responses[0].InteractionID)
			assert.Equal(t, runtime.ResumeApprove(), handle.responses[0].Resume)
		})
	}
}

func TestFreshViewerBorrowsServicesNotSessionState(t *testing.T) {
	m, _ := sessionModel(t)
	target, _ := sessionModel(t)
	m.viewers = &viewerHost{views: make(map[*app.App]*model)}
	m.status.Dormant = true
	m.status.Pending = 3
	m.lifecycle.Status = runtime.SessionStateRunning
	m.lastInterrupt = time.Now()
	m.screen.Editor.SetText("parent draft")
	m.screen.Confirm = &ui.ConfirmModel{SessionID: m.app.Session().ID, RequestID: "parent", Rejecting: true, RejectReason: "parent reason"}
	m.ownedSkillOperation = "parent operation"
	m.subagentSnapshot = m.app.Session().GetSubagentTree()
	m.pendingUsers = []ui.PendingUserMessage{{Content: "parent queued"}}
	fresh := m.newViewer(target.app, "child input")
	assert.Same(t, m.viewers, fresh.viewers)
	assert.Same(t, m.r, fresh.r)
	assert.Same(t, target.app, fresh.app)
	assert.NotSame(t, m.screen, fresh.screen)
	assert.NotSame(t, m.usage, fresh.usage)
	assert.NotSame(t, m.sessionState, fresh.sessionState)
	assert.False(t, fresh.busy())
	assert.False(t, fresh.status.Dormant)
	assert.Zero(t, fresh.status.Pending)
	assert.True(t, fresh.lastInterrupt.IsZero())
	assert.Empty(t, fresh.screen.Editor.Text())
	assert.Nil(t, fresh.screen.Confirm)
	assert.Empty(t, fresh.pendingUsers)
	assert.Empty(t, fresh.ownedSkillOperation)
	assert.Nil(t, fresh.subagentSnapshot)
	assert.Empty(t, fresh.inputParentSessionID)
}

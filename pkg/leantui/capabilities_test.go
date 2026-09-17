package leantui

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/leantui/ui"
	"github.com/docker/docker-agent/pkg/plans"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/sound"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tools/builtin/plan"
	"github.com/docker/docker-agent/pkg/tui/styles"
	"github.com/docker/docker-agent/pkg/userconfig"
)

func TestDisabledSlashCommandsRejectExecution(t *testing.T) {
	for _, command := range []string{"quit", "new", "model", "attach", "respond", "some-skill", "some-prompt"} {
		t.Run(command, func(t *testing.T) {
			m := bareModel(80)
			m.disabledCommands = map[string]bool{command: true}
			require.True(t, m.handleSlash(t.Context(), "/"+command+" argument", busySubmitSteer))
			assert.False(t, m.quitting)
			assert.Contains(t, strings.Join(m.screen.Transcript.Lines(80, 0, false, nil, nil), "\n"), "is disabled")
		})
	}
}

func TestValidatedDraftAttachment(t *testing.T) {
	m, _ := sessionModel(t)
	directory := t.TempDir()
	path := filepath.Join(directory, "source fixture.txt")
	require.NoError(t, os.WriteFile(path, []byte("source fixture"), 0o600))
	m.app.Session().WorkingDir = directory
	m.attachDraft("source fixture.txt")
	m.attachDraft("source fixture.txt")
	require.Len(t, m.draftAttachments, 1)
	assert.Equal(t, path, m.draftAttachments[0].FilePath)
	m.attachDraft(directory)
	m.attachDraft("missing")
	assert.Len(t, m.draftAttachments, 1)
	m.dropAttachment(t.Context(), path)
	assert.Empty(t, m.draftAttachments)
}

func TestAttachmentRemovedBeforeSendRetainsDraft(t *testing.T) {
	m, handle := sessionModel(t)
	path := filepath.Join(t.TempDir(), "fixture.txt")
	require.NoError(t, os.WriteFile(path, []byte("fixture"), 0o600))
	m.attachDraft(path)
	require.NoError(t, os.Remove(path))
	before := len(handle.submitted)
	m.submitEditor(t.Context(), "keep the draft")
	assert.Len(t, handle.submitted, before)
	assert.Len(t, m.draftAttachments, 1)
	assert.Equal(t, "keep the draft", m.screen.Editor.Text())
}

func TestElicitationJSONValidationCorrelationAndExactlyOnce(t *testing.T) {
	m, handle := sessionModel(t)
	event := &runtime.ElicitationRequestEvent{
		SessionID: handle.id, RequestID: "request", ElicitationID: "elicitation",
		Message: "Provide a name", Schema: map[string]any{
			"type": "object", "required": []string{"name"},
			"properties": map[string]any{"name": map[string]any{"type": "string", "minLength": 1}},
		},
	}
	m.handleEvent(t.Context(), event)
	m.respondInteraction(t.Context(), `request {}`)
	assert.Empty(t, handle.responses)
	m.respondInteraction(t.Context(), `request {"name":"Ada"}`)
	require.Len(t, handle.responses, 1)
	assert.Equal(t, "request", handle.responses[0].InteractionID)
	assert.Equal(t, "elicitation", handle.responses[0].ElicitationID)
	assert.Equal(t, tools.ElicitationActionAccept, handle.responses[0].Elicitation.Action)
	m.respondInteraction(t.Context(), `request {"name":"twice"}`)
	assert.Len(t, handle.responses, 1)
}

func TestElicitationCancelAndMaxIterations(t *testing.T) {
	m, handle := sessionModel(t)
	m.handleEvent(t.Context(), &runtime.ElicitationRequestEvent{SessionID: handle.id, RequestID: "oauth", ElicitationID: "auth", URL: "https://example.invalid/authorize"})
	m.respondInteraction(t.Context(), "oauth cancel")
	require.Len(t, handle.responses, 1)
	assert.Equal(t, tools.ElicitationActionCancel, handle.responses[0].Elicitation.Action)
	m.handleEvent(t.Context(), runtime.MaxIterationsReachedForSession(10, handle.id, "limit"))
	m.respondInteraction(t.Context(), "limit continue")
	require.Len(t, handle.responses, 2)
	assert.Equal(t, runtime.InteractionMaxIterations, handle.responses[1].Kind)
	assert.Equal(t, runtime.ResumeTypeApprove, handle.responses[1].Resume.Type)
}

func TestToolConfirmationRejectReason(t *testing.T) {
	m, handle := sessionModel(t)
	m.screen.Confirm = &ui.ConfirmModel{SessionID: handle.id, RequestID: "tool", Tool: "shell"}
	m.handleConfirmKey(t.Context(), ui.Key{Typ: ui.KeyRune, Runes: []rune{'r'}})
	m.handleConfirmKey(t.Context(), ui.Key{Typ: ui.KeyPaste, Runes: []rune("not this file")})
	m.handleConfirmKey(t.Context(), ui.Key{Typ: ui.KeyEnter})
	require.Len(t, handle.responses, 1)
	assert.Equal(t, runtime.ResumeTypeReject, handle.responses[0].Resume.Type)
	assert.Nil(t, m.screen.Confirm)
}

func TestInterruptConfirmationDoesNotCancelUntilApproved(t *testing.T) {
	m, handle := sessionModel(t)
	m.lifecycle.Status = runtime.SessionStateRunning
	m.interruptMode = "always"
	m.handleKey(t.Context(), ui.Key{Typ: ui.KeyEsc})
	assert.Zero(t, handle.stops)
	assert.True(t, m.interruptPending)
	m.handleKey(t.Context(), ui.Key{Typ: ui.KeyRune, Runes: []rune{'n'}})
	assert.Zero(t, handle.stops)
	m.handleKey(t.Context(), ui.Key{Typ: ui.KeyEsc})
	m.handleKey(t.Context(), ui.Key{Typ: ui.KeyRune, Runes: []rune{'y'}})
	assert.Equal(t, 1, handle.stops)
}

func TestBuiltinDescriptorsAreUnique(t *testing.T) {
	seen := make(map[string]bool)
	for _, command := range builtinCommands() {
		assert.False(t, seen[command.Name], command.Name)
		seen[command.Name] = true
		assert.NotEmpty(t, command.Desc)
	}
	for _, command := range []string{"attach", "drop", "subagents", "subagent-view", "subagent-attach", "back", "plans", "settings", "fork", "pause", "permissions", "respond"} {
		assert.True(t, seen[command], command)
	}
}

func TestPlanCommandsUseRevisionAndExplicitDeleteConfirmation(t *testing.T) {
	m := bareModel(100)
	m.plansService = plans.NewService(plan.NewFilesystemStorage(t.TempDir()))
	m.handlePlans(t.Context(), "create review first draft")
	created, err := m.plansService.Get(t.Context(), plans.SharedRef("review"))
	require.NoError(t, err)
	require.NotNil(t, created.Version)
	m.handlePlans(t.Context(), "edit review 0 stale draft")
	unchanged, err := m.plansService.Get(t.Context(), plans.SharedRef("review"))
	require.NoError(t, err)
	assert.Equal(t, "first draft", unchanged.Content)
	m.handlePlans(t.Context(), "delete review 1")
	_, err = m.plansService.Get(t.Context(), plans.SharedRef("review"))
	require.NoError(t, err)
	m.handlePlans(t.Context(), "delete review 1 confirm")
	_, err = m.plansService.Get(t.Context(), plans.SharedRef("review"))
	require.Error(t, err)
}

func TestUnknownSettingsNeverAccessPersistence(t *testing.T) {
	m := bareModel(100)
	m.handleSettings("unknown true")
	assert.Contains(t, strings.Join(m.screen.Transcript.Lines(100, 0, false, nil, nil), "\n"), "unknown setting")
}

func TestBusyTranscriptHasNoGlobalWorkingSpinner(t *testing.T) {
	m := bareModel(100)
	m.lifecycle.Status = runtime.SessionStateRunning
	m.status.Agent = "worker"
	lines, _, _ := m.buildLines()
	text := strings.Join(lines, "\n")
	assert.NotContains(t, text, "Working")
	assert.Contains(t, text, "worker")
	assert.Contains(t, text, "active")
	assert.Empty(t, m.screen.Transcript.Lines(100, 0, true, nil, nil))
}

func TestInterruptModesDoubleTapAndNone(t *testing.T) {
	m, handle := sessionModel(t)
	m.lifecycle.Status = runtime.SessionStateRunning
	m.interruptMode = "double-tap"
	m.handleKey(t.Context(), ui.Key{Typ: ui.KeyEsc})
	assert.Zero(t, handle.stops)
	m.lastInterrupt = time.Now().Add(-2 * time.Second)
	m.handleKey(t.Context(), ui.Key{Typ: ui.KeyEsc})
	assert.Zero(t, handle.stops, "expired first press must rearm")
	m.handleKey(t.Context(), ui.Key{Typ: ui.KeyEsc})
	assert.Equal(t, 1, handle.stops)
	m.interruptMode = "none"
	m.handleKey(t.Context(), ui.Key{Typ: ui.KeyEsc})
	assert.Equal(t, 2, handle.stops)
}

func TestInteractiveShellUsesHandoffAndKeepsDraftOnFailure(t *testing.T) {
	m, _ := sessionModel(t)
	m.app.Session().WorkingDir = t.TempDir()
	m.screen.Editor.SetText("unsent draft")
	called := false
	m.runExternal = func(command *exec.Cmd) error {
		called = true
		assert.Equal(t, m.app.Session().WorkingDir, command.Dir)
		return errors.New("fixture handoff failure")
	}
	assert.True(t, m.handleSlash(t.Context(), "/shell", busySubmitSteer))
	assert.True(t, called)
	assert.Equal(t, "unsent draft", m.screen.Editor.Text())
	assert.Contains(t, strings.Join(m.screen.Transcript.Lines(100, 0, false, nil, nil), "\n"), "fixture handoff failure")
}

func TestSettingsPersistAndApplyWithoutUserConfiguration(t *testing.T) {
	m := bareModel(100)
	config := &userconfig.Config{}
	m.settingsSave = func(mutate func(*userconfig.Config) error) error { return mutate(config) }
	m.handleSettings("send-mode queue")
	require.NotNil(t, config.Settings)
	assert.Equal(t, "queue", config.Settings.BusySendMode)
	assert.True(t, m.queueSendMode)
	m.handleSettings("interrupt-confirmation always")
	assert.Equal(t, "always", m.interruptMode)
	m.handleSettings("render-images false")
	assert.False(t, m.renderImages)
	m.handleSettings("snapshot true")
	require.NotNil(t, config.Settings.Snapshot)
	assert.True(t, *config.Settings.Snapshot)
	m.settingsSave = func(func(*userconfig.Config) error) error { return errors.New("fixture write denied") }
	m.handleSettings("send-mode steer")
	assert.True(t, m.queueSendMode, "failed persistence does not partially apply settings")
}

func TestNotificationSoundUsesCanonicalSettlementsAndThreshold(t *testing.T) {
	m, handle := sessionModel(t)
	m.soundEnabled = true
	m.soundThreshold = time.Second
	var played []sound.Event
	m.playSound = func(_ context.Context, event sound.Event) { played = append(played, event) }
	m.streamStarted = time.Now().Add(-2 * time.Second)
	event := &runtime.TurnSettledEvent{SessionID: handle.id, TurnID: "success", Outcome: runtime.TurnCompleted}
	m.handleEvent(t.Context(), app.SessionEventMsg{OriginSessionID: handle.id, Sequence: 1, Event: event})
	assert.Equal(t, []sound.Event{sound.Success}, played)
	m.handleEvent(t.Context(), app.SessionEventMsg{OriginSessionID: handle.id, Sequence: 1, Event: event})
	assert.Len(t, played, 1)
	m.streamStarted = time.Now()
	m.handleEvent(t.Context(), app.SessionEventMsg{OriginSessionID: handle.id, Sequence: 2, Event: &runtime.TurnSettledEvent{SessionID: handle.id, TurnID: "fast", Outcome: runtime.TurnCompleted}})
	assert.Len(t, played, 1)
	m.handleEvent(t.Context(), app.SessionEventMsg{OriginSessionID: handle.id, Sequence: 3, Event: &runtime.TurnSettledEvent{SessionID: handle.id, TurnID: "cancel", Outcome: runtime.TurnCanceled}})
	assert.Len(t, played, 1)
	m.handleEvent(t.Context(), app.SessionEventMsg{OriginSessionID: handle.id, Sequence: 4, Event: &runtime.TurnSettledEvent{SessionID: handle.id, TurnID: "failed", Outcome: runtime.TurnFailed}})
	assert.Equal(t, []sound.Event{sound.Success, sound.Failure}, played)
}

func TestGlobalSettingsBroadcastWithoutChangingViewerState(t *testing.T) {
	m, _ := sessionModel(t)
	hidden, _ := sessionModel(t)
	m.viewers = &viewerHost{views: map[*app.App]*model{hidden.app: hidden}}
	m.settingsSave = func(mutate func(*userconfig.Config) error) error { return mutate(&userconfig.Config{}) }
	hidden.screen.Editor.SetText("hidden draft")
	hidden.lifecycle.Status = runtime.SessionStateRunning
	hidden.interruptPending = true
	hidden.screen.Transcript.AddUser("hidden history")
	screen, transcript := hidden.screen, hidden.screen.Transcript
	m.handleSettings("send-mode queue")
	m.handleSettings("sound true")
	m.handleSettings("interrupt-confirmation double-tap")
	m.handleSettings("render-images false")
	assert.True(t, m.queueSendMode)
	assert.True(t, hidden.queueSendMode)
	assert.True(t, hidden.soundEnabled)
	assert.Equal(t, "double-tap", hidden.interruptMode)
	assert.False(t, hidden.renderImages)
	assert.True(t, hidden.busy())
	assert.True(t, hidden.interruptPending)
	assert.Equal(t, "hidden draft", hidden.screen.Editor.Text())
	assert.Same(t, screen, hidden.screen)
	assert.Same(t, transcript, hidden.screen.Transcript)
	assert.Equal(t, 1, hidden.screen.Transcript.BlockCount())
}

func TestTerminalInitializationFailureClosesInitialPresentationAndRuntimeOnce(t *testing.T) {
	m, handle := sessionModel(t)
	ready, stopped := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(stopped)
		m.app.Subscribe(t.Context(), func(any) {}, app.SubscribeOptions{Ready: ready})
	}()
	<-ready
	cleanupCalls := 0
	failure := errors.New("fixture terminal unavailable")
	err := runWithTerminalFactory(t.Context(), Config{App: m.app, Cleanup: func() { cleanupCalls++ }}, func(*os.File, *os.File) (*ui.Terminal, error) {
		return nil, failure
	})
	require.ErrorIs(t, err, failure)
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("initial presentation subscription was not closed")
	}
	assert.Equal(t, 1, cleanupCalls)
	assert.Zero(t, handle.stops, "presentation teardown must not cancel canonical execution")
}

func TestThemeSettingsAndAutomaticBackgroundInvalidateHiddenPresentation(t *testing.T) {
	originalAuto, originalDark := styles.AutoThemeEnabled(), styles.TerminalIsDark()
	t.Cleanup(func() { styles.SetAutoThemeEnabled(originalAuto); styles.SetTerminalDark(originalDark) })
	m := bareModel(80)
	hidden := bareModel(80)
	m.viewers = &viewerHost{views: map[*app.App]*model{nil: hidden}}
	config := &userconfig.Config{}
	m.settingsSave = func(mutate func(*userconfig.Config) error) error { return mutate(config) }
	m.themeResolve = func(ref string) string {
		if ref == "auto" {
			if styles.TerminalIsDark() {
				return "fixture-dark"
			}
			return "fixture-light"
		}
		return ref
	}
	m.themeLoad = func(ref string) (*styles.Theme, error) { return &styles.Theme{Ref: ref}, nil }
	var applied []string
	m.themeApply = func(theme *styles.Theme) { applied = append(applied, theme.Ref) }
	renders := 0
	hidden.screen.Transcript.AddBlock(func(int) []string { renders++; return []string{"fixture"} })
	hidden.screen.Transcript.Lines(80, 0, false, nil, nil)
	hidden.screen.Editor.SetText("retained")
	m.handleSettings("theme fixture-dark")
	assert.Equal(t, "fixture-dark", config.Settings.Theme)
	hidden.screen.Transcript.Lines(80, 0, false, nil, nil)
	assert.Equal(t, 2, renders)
	m.handleSettings("theme auto")
	m.handleKey(t.Context(), ui.Key{Typ: ui.KeyBackgroundLight})
	assert.Equal(t, "fixture-light", applied[len(applied)-1])
	assert.Equal(t, "retained", hidden.screen.Editor.Text())
}

type fixtureThemeWatcher struct {
	changed func(string)
	watched string
	stops   int
}

func (w *fixtureThemeWatcher) Watch(ref string) error { w.watched = ref; return nil }
func (w *fixtureThemeWatcher) Stop()                  { w.stops++ }

func TestThemeHotReloadRejectsRetiredSelectionAndStopsExactlyOnce(t *testing.T) {
	m := bareModel(80)
	var watchers []*fixtureThemeWatcher
	m.viewers = &viewerHost{ctx: t.Context, events: make(chan any, 8), views: make(map[*app.App]*model), themeWatcherFactory: func(changed func(string)) ThemeWatcher {
		watcher := &fixtureThemeWatcher{changed: changed}
		watchers = append(watchers, watcher)
		return watcher
	}}
	loads := 0
	m.themeLoad = func(ref string) (*styles.Theme, error) { loads++; return &styles.Theme{Ref: ref}, nil }
	applies := 0
	m.themeApply = func(*styles.Theme) { applies++ }
	m.retargetThemeWatcher("fixture")
	require.Len(t, watchers, 1)
	watchers[0].changed("fixture")
	stale := <-m.viewers.events
	m.retargetThemeWatcher("fixture")
	require.Len(t, watchers, 2)
	assert.Equal(t, 1, watchers[0].stops)
	m.routeViewerEvent(t.Context(), stale)
	assert.Zero(t, loads, "same-ref reselection must reject prior watcher generation")
	watchers[1].changed("fixture")
	m.routeViewerEvent(t.Context(), <-m.viewers.events)
	assert.Equal(t, 1, loads)
	assert.Equal(t, 1, applies)
	m.viewers.close()
	m.viewers.close()
	assert.Equal(t, 1, watchers[1].stops)
	m.routeViewerEvent(t.Context(), themeFileChanged{ref: "fixture", generation: m.viewers.themeGeneration})
	assert.Equal(t, 1, applies)
}

func TestStandalonePanesIsExplicitlyUnavailableAndNeverAgentInput(t *testing.T) {
	for _, argument := range []string{"", "right", "down session-id", "next", "prev", "remove", "single", "resize divider-id"} {
		t.Run(argument, func(t *testing.T) {
			m := bareModel(100)
			// No App: any fallthrough to agent resolution or submission would
			// panic instead of returning the required local capability notice.
			require.True(t, m.handleSlash(t.Context(), "/panes "+argument, busySubmitSteer))
			text := strings.Join(m.screen.Transcript.Lines(100, 0, false, nil, nil), "\n")
			assert.Contains(t, text, "unavailable in standalone lean")
			assert.Empty(t, m.pendingUsers)
			assert.Empty(t, m.queue)
		})
	}
	m := bareModel(100)
	m.disabledCommands = map[string]bool{"panes": true}
	require.True(t, m.handleSlash(t.Context(), "/panes right", busySubmitSteer))
	assert.Contains(t, strings.Join(m.screen.Transcript.Lines(100, 0, false, nil, nil), "\n"), "Command /panes is disabled")
}

func TestDormancyUsesCanonicalSnapshotAndAddressedEvents(t *testing.T) {
	m, handle := sessionModel(t)
	for _, pending := range []int{0, 2} {
		m.handleEvent(t.Context(), &app.SessionResetEvent{Snapshot: runtime.SessionSnapshot{Session: m.app.Session(), Status: runtime.SessionStatus{SessionID: handle.id, Dormant: true, Pending: pending}}})
		lines, _, _ := m.buildLines()
		text := strings.Join(lines, "\n")
		assert.Contains(t, text, "Restored · paused")
		assert.Contains(t, text, "/resume")
		assert.Contains(t, text, "Any queued work or reports will wait until you resume this session.")
		assert.NotContains(t, text, "reports queued", "no report count or presence is inferred")
		m.handleEvent(t.Context(), &runtime.DormancyChangedEvent{SessionID: "relative", Dormant: false})
		assert.True(t, m.status.Dormant)
		m.handleEvent(t.Context(), &runtime.DormancyChangedEvent{SessionID: handle.id, Dormant: false})
		assert.False(t, m.status.Dormant)
	}
}

func TestUnsupportedResumeKeepsAuthoritativeDormancyAndDraft(t *testing.T) {
	m, handle := sessionModel(t)
	m.status.Dormant = true
	m.screen.Editor.SetText("retained draft")
	before := len(handle.submitted)
	require.True(t, m.handleSlash(t.Context(), "/resume", busySubmitSteer))
	assert.True(t, m.status.Dormant, "only canonical events may change dormancy presentation")
	assert.Equal(t, "retained draft", m.screen.Editor.Text())
	assert.Len(t, handle.submitted, before, "resume is never a synthetic input")
	assert.Zero(t, handle.stops)
}

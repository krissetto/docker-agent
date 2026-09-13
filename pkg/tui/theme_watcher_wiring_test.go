package tui

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/docker/docker-agent/pkg/tui/styles"
)

// fakeThemeWatcher records Watch/Stop calls so the wiring between the model
// lifecycle and the theme watcher can be asserted without touching the
// filesystem.
type fakeThemeWatcher struct {
	watched []string
	stopped bool
}

func (f *fakeThemeWatcher) Watch(themeRef string) error {
	f.watched = append(f.watched, themeRef)
	return nil
}

func (f *fakeThemeWatcher) Stop() { f.stopped = true }

func TestWatchCurrentTheme_TargetsAppliedTheme(t *testing.T) {
	m, _ := newTestModel(t)
	fw := &fakeThemeWatcher{}
	m.themeWatcher = fw

	m.watchCurrentTheme()

	assert.Equal(t, []string{styles.CurrentTheme().Ref}, fw.watched)
}

func TestWatchCurrentTheme_NoWatcherIsNoop(t *testing.T) {
	m, _ := newTestModel(t)

	assert.NotPanics(t, func() { m.watchCurrentTheme() })
}

func TestApplyThemeChanged_RetargetsWatcher(t *testing.T) {
	m, _ := newTestModel(t)
	fw := &fakeThemeWatcher{}
	m.themeWatcher = fw

	_, _ = m.applyThemeChanged()

	assert.Equal(t, []string{styles.CurrentTheme().Ref}, fw.watched,
		"theme changes must re-target the watcher onto the active theme")
}

func TestCleanupManagedResources_StopsThemeWatcher(t *testing.T) {
	m, _ := newTestModel(t)
	fw := &fakeThemeWatcher{}
	m.themeWatcher = fw

	m.cleanupManagedResources()

	assert.True(t, fw.stopped, "TUI shutdown must stop the theme watcher")
}

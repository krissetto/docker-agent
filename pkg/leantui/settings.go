package leantui

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/docker/docker-agent/pkg/leantui/ui"
	"github.com/docker/docker-agent/pkg/tui/styles"
	"github.com/docker/docker-agent/pkg/userconfig"
)

// Settings uses the same persisted host settings as the full TUI. Options
// requiring runtime reconstruction are explicitly marked next-launch settings.
func (m *model) handleSettings(arg string) {
	key, value, _ := strings.Cut(arg, " ")
	if key == "theme" || key == "theme-dark" || key == "theme-light" {
		m.handleThemeSetting(key, strings.TrimSpace(value))
		return
	}
	if key == "" {
		m.reportCapability("/settings theme [ref], theme-dark <ref>, theme-light <ref>; /settings <key> <value>: send-mode steer|queue; interrupt-confirmation always|double-tap|none; render-images, show-banner, expand-thinking, hide-tool-results, split-diff true|false (applied now); snapshot, cache-stable-prompts, warn-on-cache-miss, restore-tabs, lean true|false (next launch); sound true|false and sound-threshold <seconds> (applied now). Safety changes use /yolo confirm. Pane geometry and the tour require the full TUI.", nil)
		return
	}
	value = strings.TrimSpace(value)
	var apply func(*userconfig.Settings)
	var live func(*model)
	switch key {
	case "interrupt-confirmation":
		if value != "always" && value != "double-tap" && value != "none" {
			m.reportCapability(nil, errors.New("interrupt-confirmation must be always, double-tap, or none"))
			return
		}
		apply = func(settings *userconfig.Settings) { settings.InterruptConfirmation = value }
		live = func(viewer *model) { viewer.interruptMode = value }
	case "send-mode":
		if value != "steer" && value != "queue" {
			m.reportCapability(nil, errors.New("send-mode must be steer or queue"))
			return
		}
		apply = func(settings *userconfig.Settings) { settings.BusySendMode = value }
		live = func(viewer *model) { viewer.queueSendMode = value == "queue" }
	case "sound-threshold":
		seconds, err := strconv.Atoi(value)
		if err != nil || seconds < 0 {
			m.reportCapability(nil, errors.New("sound-threshold must be a nonnegative number of seconds"))
			return
		}
		apply = func(settings *userconfig.Settings) { settings.SoundThreshold = seconds }
		live = func(viewer *model) { viewer.soundThreshold = time.Duration(seconds) * time.Second }
	default:
		enabled, err := strconv.ParseBool(value)
		if err != nil {
			m.reportCapability(nil, fmt.Errorf("%s expects true or false", key))
			return
		}
		switch key {
		case "render-images":
			apply = func(s *userconfig.Settings) { s.RenderImages = &enabled }
			live = func(viewer *model) { viewer.renderImages = enabled }
		case "show-banner":
			apply = func(s *userconfig.Settings) { s.ShowBanner = &enabled }
			live = func(viewer *model) { viewer.hideBanner = !enabled }
		case "expand-thinking":
			apply = func(s *userconfig.Settings) { s.ExpandThinking = &enabled }
			live = func(viewer *model) { viewer.sessionState.SetExpandThinking(enabled) }
		case "hide-tool-results":
			apply = func(s *userconfig.Settings) { s.HideToolResults = enabled }
			live = func(viewer *model) { viewer.sessionState.SetHideToolResults(enabled) }
		case "split-diff":
			apply = func(s *userconfig.Settings) { s.SplitDiffView = &enabled }
			live = func(viewer *model) { viewer.sessionState.SetSplitDiffView(enabled) }
		case "snapshot":
			apply = func(s *userconfig.Settings) { s.Snapshot = &enabled }
		case "cache-stable-prompts":
			apply = func(s *userconfig.Settings) { s.CacheStablePrompts = &enabled }
		case "warn-on-cache-miss":
			apply = func(s *userconfig.Settings) { s.WarnOnCacheMiss = &enabled }
		case "restore-tabs":
			apply = func(s *userconfig.Settings) { s.RestoreTabs = &enabled }
		case "sound":
			apply = func(s *userconfig.Settings) { s.Sound = enabled }
			live = func(viewer *model) { viewer.soundEnabled = enabled }
		case "lean":
			apply = func(s *userconfig.Settings) { s.Lean = enabled }
		default:
			m.reportCapability(nil, fmt.Errorf("unknown setting %q; /settings lists supported keys", key))
			return
		}
	}
	save := m.settingsSave
	if save == nil {
		save = userconfig.Update
	}
	err := save(func(config *userconfig.Config) error {
		if config.Settings == nil {
			config.Settings = &userconfig.Settings{}
		}
		apply(config.Settings)
		return nil
	})
	if err != nil {
		m.reportCapability(nil, err)
		return
	}
	if live != nil {
		live(m)
		m.invalidatePresentation()
		if m.viewers != nil {
			for _, hidden := range m.viewers.views {
				live(hidden)
				hidden.invalidatePresentation()
			}
		}
		m.reportCapability("Setting saved and applied.", nil)
	} else {
		m.reportCapability("Setting saved; applies on the next launch.", nil)
	}
}

func (m *model) invalidatePresentation() {
	m.screen.Transcript.InvalidateCaches()
	if m.r != nil {
		m.r.Repaint()
	}
}

func (m *model) handleThemeSetting(key, ref string) {
	list, load, applyTheme := m.themeList, m.themeLoad, m.themeApply
	if list == nil {
		list = styles.ListThemeRefs
	}
	if load == nil {
		load = styles.LoadTheme
	}
	if applyTheme == nil {
		applyTheme = styles.ApplyTheme
	}
	if ref == "" {
		refs, err := list()
		if err != nil {
			m.reportCapability(nil, err)
			return
		}
		choices := []ui.Command{{Name: "auto", Desc: "Match terminal background", Value: "auto"}}
		for _, candidate := range refs {
			choices = append(choices, ui.Command{Name: candidate, Value: candidate})
		}
		m.completeArgument("settings "+key, choices)
		return
	}
	if key != "theme" && ref == styles.AutoThemeRef {
		m.reportCapability("theme-dark and theme-light require a concrete theme reference.", nil)
		return
	}
	resolved := ref
	if ref == styles.AutoThemeRef {
		resolved = m.resolveTheme(ref)
	}
	theme, err := load(resolved)
	if err != nil {
		m.reportCapability(nil, err)
		return
	}
	save := m.settingsSave
	if save == nil {
		save = userconfig.Update
	}
	err = save(func(config *userconfig.Config) error {
		if config.Settings == nil {
			config.Settings = &userconfig.Settings{}
		}
		switch key {
		case "theme":
			config.Settings.Theme = ref
		case "theme-dark":
			config.Settings.ThemeDark = ref
		case "theme-light":
			config.Settings.ThemeLight = ref
		}
		return nil
	})
	if err != nil {
		m.reportCapability(nil, err)
		return
	}
	active := key == "theme" || (styles.AutoThemeEnabled() && ((key == "theme-dark" && styles.TerminalIsDark()) || (key == "theme-light" && !styles.TerminalIsDark())))
	if key == "theme" {
		styles.SetAutoThemeEnabled(ref == styles.AutoThemeRef)
	}
	if active {
		applyTheme(theme)
		m.retargetThemeWatcher(theme.Ref)
		m.invalidatePresentation()
		if m.viewers != nil {
			for _, hidden := range m.viewers.views {
				hidden.invalidatePresentation()
			}
		}
	}
	m.reportCapability("Theme preference saved.", nil)
}

func (m *model) handleBackgroundColor(dark bool) {
	styles.SetTerminalDark(dark)
	if !styles.AutoThemeEnabled() {
		return
	}
	load, apply := m.themeLoad, m.themeApply
	if load == nil {
		load = styles.LoadTheme
	}
	if apply == nil {
		apply = styles.ApplyTheme
	}
	theme, err := load(m.resolveTheme(styles.AutoThemeRef))
	if err != nil {
		m.reportCapability(nil, err)
		return
	}
	if current := styles.CurrentTheme(); current != nil && current.Ref == theme.Ref {
		return
	}
	apply(theme)
	m.retargetThemeWatcher(theme.Ref)
	m.invalidatePresentation()
	if m.viewers != nil {
		for _, hidden := range m.viewers.views {
			hidden.invalidatePresentation()
		}
	}
}

func (m *model) resolveTheme(ref string) string {
	if m.themeResolve != nil {
		return m.themeResolve(ref)
	}
	return styles.ResolveThemeRef(ref)
}

// ThemeWatcher is the existing styles file-watcher contract. Only one active
// watcher belongs to the frontend, regardless of how many viewers are open.
type ThemeWatcher interface {
	Watch(themeRef string) error
	Stop()
}

type themeFileChanged struct {
	ref        string
	generation uint64
}

func (m *model) retargetThemeWatcher(ref string) {
	host := m.viewers
	if host == nil || host.closed || host.events == nil {
		return
	}
	if host.themeWatcher != nil {
		host.themeWatcher.Stop()
		host.themeWatcher = nil
	}
	host.themeGeneration++
	generation := host.themeGeneration
	host.themeRef = ref
	factory := host.themeWatcherFactory
	if factory == nil {
		factory = func(changed func(string)) ThemeWatcher { return styles.NewThemeWatcher(changed) }
	}
	watcher := factory(func(changed string) {
		select {
		case host.events <- themeFileChanged{ref: changed, generation: generation}:
		case <-host.ctx().Done():
		}
	})
	host.themeWatcher = watcher
	if err := watcher.Watch(ref); err != nil {
		m.reportCapability(nil, err)
	}
}

func (m *model) handleThemeFileChanged(event themeFileChanged) {
	host := m.viewers
	if host == nil || host.closed || event.generation != host.themeGeneration || event.ref != host.themeRef {
		return
	}
	load, apply := m.themeLoad, m.themeApply
	if load == nil {
		load = styles.LoadTheme
	}
	if apply == nil {
		apply = styles.ApplyTheme
	}
	styles.InvalidateThemeCache(event.ref)
	theme, err := load(event.ref)
	if err != nil {
		m.reportCapability(nil, err)
		return
	}
	apply(theme)
	m.invalidatePresentation()
	for _, hidden := range host.views {
		hidden.invalidatePresentation()
	}
	m.reportCapability("Theme hot-reloaded.", nil)
}

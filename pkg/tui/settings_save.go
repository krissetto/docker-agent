package tui

import (
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/userconfig"
)

// Merge only the fields edited by this draft into the freshly locked config.
func saveChangedPreferences(original, draft messages.Preferences) error {
	return userconfig.Update(func(cfg *userconfig.Config) error {
		latest := preferencesFromConfig(cfg.GetSettings())
		if original.SendMode != draft.SendMode {
			latest.SendMode = draft.SendMode
		}
		if original.SplitDiffView != draft.SplitDiffView {
			latest.SplitDiffView = draft.SplitDiffView
		}
		if original.ExpandThinking != draft.ExpandThinking {
			latest.ExpandThinking = draft.ExpandThinking
		}
		if original.HideToolResults != draft.HideToolResults {
			latest.HideToolResults = draft.HideToolResults
		}
		if original.RenderImages != draft.RenderImages {
			latest.RenderImages = draft.RenderImages
		}
		if original.ShowBanner != draft.ShowBanner {
			latest.ShowBanner = draft.ShowBanner
		}
		if original.DimInactivePanes != draft.DimInactivePanes {
			latest.DimInactivePanes = draft.DimInactivePanes
		}
		if original.TransparentBackground != draft.TransparentBackground {
			latest.TransparentBackground = draft.TransparentBackground
		}
		if original.YOLO != draft.YOLO {
			latest.YOLO = draft.YOLO
		}
		if original.RestoreTabs != draft.RestoreTabs {
			latest.RestoreTabs = draft.RestoreTabs
		}
		if original.Snapshot != draft.Snapshot {
			latest.Snapshot = draft.Snapshot
		}
		if original.CacheStablePrompts != draft.CacheStablePrompts {
			latest.CacheStablePrompts = draft.CacheStablePrompts
		}
		if original.WarnOnCacheMiss != draft.WarnOnCacheMiss {
			latest.WarnOnCacheMiss = draft.WarnOnCacheMiss
		}
		if original.Lean != draft.Lean {
			latest.Lean = draft.Lean
		}
		if original.TabTitleMaxLength != draft.TabTitleMaxLength {
			latest.TabTitleMaxLength = draft.TabTitleMaxLength
		}
		if original.Sound != draft.Sound {
			latest.Sound = draft.Sound
		}
		if original.SoundThreshold != draft.SoundThreshold {
			latest.SoundThreshold = draft.SoundThreshold
		}
		if original.InterruptConfirmation != draft.InterruptConfirmation {
			latest.InterruptConfirmation = draft.InterruptConfirmation
		}
		if original.Layout.SidebarPosition != draft.Layout.SidebarPosition {
			latest.Layout.SidebarPosition = draft.Layout.SidebarPosition
		}
		if original.Layout.SectionSpacing != draft.Layout.SectionSpacing {
			latest.Layout.SectionSpacing = draft.Layout.SectionSpacing
		}
		if original.Layout.SidebarInfoMode != draft.Layout.SidebarInfoMode {
			latest.Layout.SidebarInfoMode = draft.Layout.SidebarInfoMode
		}
		if original.Layout.ActiveAgentsOnly != draft.Layout.ActiveAgentsOnly {
			latest.Layout.ActiveAgentsOnly = draft.Layout.ActiveAgentsOnly
		}
		if original.Layout.HideSessionPath != draft.Layout.HideSessionPath {
			latest.Layout.HideSessionPath = draft.Layout.HideSessionPath
		}
		if original.Layout.HideUsage != draft.Layout.HideUsage {
			latest.Layout.HideUsage = draft.Layout.HideUsage
		}
		if original.Layout.HideAgents != draft.Layout.HideAgents {
			latest.Layout.HideAgents = draft.Layout.HideAgents
		}
		if original.Layout.HideTools != draft.Layout.HideTools {
			latest.Layout.HideTools = draft.Layout.HideTools
		}
		if original.Layout.HideTodos != draft.Layout.HideTodos {
			latest.Layout.HideTodos = draft.Layout.HideTodos
		}
		if !original.Panel.Equal(draft.Panel) {
			latest.Panel = messages.NormalizePanelSettings(draft.Panel)
		}
		if err := writePreferences(cfg, latest); err != nil {
			return err
		}
		if original.Theme != draft.Theme {
			cfg.Settings.Theme = draft.Theme
		}
		return nil
	})
}

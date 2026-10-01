package messages

// Theme messages control theme selection, preview, and hot-reload.
type (
	// OpenThemePickerMsg opens the theme picker dialog.
	OpenThemePickerMsg struct{ SettingsID uint64 }

	// ChangeThemeMsg applies the specified theme.
	ChangeThemeMsg struct {
		SettingsID uint64
		PickerID   uint64
		Revision   uint64
		ThemeRef   string // Theme reference to apply
	}

	// ThemePreviewMsg previews a theme without committing.
	ThemePreviewMsg struct {
		SettingsID  uint64
		PickerID    uint64
		Revision    uint64
		ThemeRef    string // Theme reference to preview
		OriginalRef string // Original theme to restore on cancel
	}

	// ThemeCancelPreviewMsg cancels theme preview and restores original.
	ThemeCancelPreviewMsg struct {
		SettingsID  uint64
		PickerID    uint64
		Revision    uint64
		OriginalRef string // Theme reference to restore
	}

	// ThemeChangedMsg notifies components that the theme has changed (for cache invalidation).
	ThemeChangedMsg struct{}

	// ThemeFileChangedMsg notifies TUI that the theme file was modified on disk (hot reload).
	// The TUI should load and apply the theme on the main goroutine to avoid race conditions.
	ThemeFileChangedMsg struct {
		ThemeRef string // The theme ref that was modified
	}
)

package styles

import (
	"sync/atomic"

	"github.com/docker/docker-agent/pkg/userconfig"
)

// AutoThemeRef is the sentinel theme reference that makes the theme follow
// the terminal's light/dark background. It is never loaded directly: callers
// resolve it to a concrete theme with ResolveThemeRef first.
const AutoThemeRef = "auto"

// DefaultLightThemeRef is the built-in light counterpart of the default
// theme, applied when the auto theme detects a light terminal background and
// no settings.theme_light is configured.
const DefaultLightThemeRef = DockerNextLightThemeRef

// AutoThemeDisplayName is the label shown for the auto theme in the picker
// and in notifications.
const AutoThemeDisplayName = "Auto (match terminal)"

// terminalIsLight holds the detected terminal background polarity. The zero
// value (false) means dark, so an undetected terminal keeps today's dark
// default.
var terminalIsLight atomic.Bool

// SetTerminalDark records the detected terminal background polarity.
func SetTerminalDark(dark bool) {
	terminalIsLight.Store(!dark)
}

// TerminalIsDark reports the last detected terminal background polarity.
// Defaults to dark when no detection ever ran.
func TerminalIsDark() bool {
	return !terminalIsLight.Load()
}

// autoThemeEnabled tracks whether the auto theme is the active selection
// (via settings.theme, --theme auto, or the picker). The TUI consults it to
// enable terminal color-scheme reporting and to react to polarity changes.
var autoThemeEnabled atomic.Bool

// SetAutoThemeEnabled records whether the auto theme is the active selection.
func SetAutoThemeEnabled(enabled bool) {
	autoThemeEnabled.Store(enabled)
}

// AutoThemeEnabled reports whether the auto theme is the active selection.
func AutoThemeEnabled() bool {
	return autoThemeEnabled.Load()
}

// ResolveThemeRef maps the AutoThemeRef sentinel to the concrete theme for
// the current terminal polarity: settings.theme_dark (default "docker-next-dark") on
// a dark background, settings.theme_light (default "docker-next-light") on a
// light one. Any other ref passes through unchanged.
func ResolveThemeRef(ref string) string {
	if ref != AutoThemeRef {
		return ref
	}
	settings := userconfig.Get()
	if TerminalIsDark() {
		if settings.ThemeDark != "" {
			return settings.ThemeDark
		}
		return DefaultThemeRef
	}
	if settings.ThemeLight != "" {
		return settings.ThemeLight
	}
	return DefaultLightThemeRef
}

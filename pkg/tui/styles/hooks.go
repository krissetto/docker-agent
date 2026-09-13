package styles

import (
	"sync"
	"sync/atomic"
)

// themeChangeHooks are run at the end of every ApplyTheme, after all style
// variables have been rebuilt. Packages that memoize rendering derived from
// these styles (e.g. the markdown renderer's style cache) register an
// invalidation hook at init time, so no ApplyTheme caller can forget to
// reset them.
var (
	themeGeneration    atomic.Uint64
	themeChangeHooksMu sync.Mutex
	themeChangeHooks   []func()
)

// OnThemeChange registers fn to run after every theme application. It is
// meant to be called once per package, typically from init.
func OnThemeChange(fn func()) {
	themeChangeHooksMu.Lock()
	defer themeChangeHooksMu.Unlock()
	themeChangeHooks = append(themeChangeHooks, fn)
}

// ThemeGeneration identifies the current palette for component-local render caches.
func ThemeGeneration() uint64 { return themeGeneration.Load() }

// runThemeChangeHooks runs all registered theme-change hooks.
func runThemeChangeHooks() {
	themeGeneration.Add(1)
	themeChangeHooksMu.Lock()
	hooks := make([]func(), len(themeChangeHooks))
	copy(hooks, themeChangeHooks)
	themeChangeHooksMu.Unlock()
	for _, fn := range hooks {
		fn()
	}
}

// ApplyThemeRef loads the theme by reference and applies it, falling back
// to the default theme when the reference does not resolve. It is the
// one-call entry point for embedders that align their host theme with
// docker-agent's components and only know a theme name. The applied theme
// is returned.
func ApplyThemeRef(ref string) *Theme {
	theme, err := LoadTheme(ref)
	if err != nil {
		theme = DefaultTheme()
	}
	ApplyTheme(theme)
	return theme
}

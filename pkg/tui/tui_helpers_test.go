package tui

import (
	"context"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/app"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/commands"
	"github.com/docker/docker-agent/pkg/tui/components/tabbar"
)

func TestKeyboardEnhancementsUpdateCompleteHelp(t *testing.T) {
	m, _ := newTestModel(t)
	m.focusedPanel = PanelEditor
	m.tabBar = tabbar.New(animation.NewRuntime(), 0)

	require.Equal(t, []string{"ctrl+j"}, helpKeys(m.helpDocument().Current, "composer.newline"))
	_, _ = m.Update(tea.KeyboardEnhancementsMsg{Flags: 1})
	require.Equal(t, []string{"shift+enter", "ctrl+j"}, helpKeys(m.helpDocument().Current, "composer.newline"))
}

func TestHitTestLeanRegion(t *testing.T) {
	t.Parallel()

	const contentHeight = 20

	tests := []struct {
		name string
		y    int
		want layoutRegion
	}{
		{name: "top of screen", y: 0, want: regionContent},
		{name: "middle of content", y: 10, want: regionContent},
		{name: "last row of content", y: 19, want: regionContent},
		{name: "first row of editor", y: 20, want: regionEditor},
		{name: "deep in editor", y: 100, want: regionEditor},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := hitTestLeanRegion(tt.y, contentHeight); got != tt.want {
				t.Errorf("hitTestLeanRegion(%d, %d) = %v, want %v", tt.y, contentHeight, got, tt.want)
			}
		})
	}
}

func TestHitTestFullRegion(t *testing.T) {
	t.Parallel()

	// Layout (with tab bar present):
	//   y=[0..19]  content       (contentHeight = 20)
	//   y=[20]     resize handle (1 row)
	//   y=[21..22] tab bar       (tabBarHeight = 2)
	//   y=[23..27] editor        (editorHeight = 5)
	//   y>=28      context usage
	const (
		contentHeight = 20
		tabBarHeight  = 2
		editorHeight  = 5
	)

	tests := []struct {
		name string
		y    int
		want layoutRegion
	}{
		{name: "top of content", y: 0, want: regionContent},
		{name: "last row of content", y: 19, want: regionContent},
		{name: "resize handle", y: 20, want: regionResizeHandle},
		{name: "first row of tab bar", y: 21, want: regionTabBar},
		{name: "last row of tab bar", y: 22, want: regionTabBar},
		{name: "first row of editor", y: 23, want: regionEditor},
		{name: "last row of editor", y: 27, want: regionEditor},
		{name: "context usage", y: 28, want: regionContextUsage},
		{name: "far below the screen", y: 100, want: regionOutside},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := hitTestFullRegion(tt.y, contentHeight, tabBarHeight, editorHeight)
			if got != tt.want {
				t.Errorf("hitTestFullRegion(%d, %d, %d, %d) = %v, want %v",
					tt.y, contentHeight, tabBarHeight, editorHeight, got, tt.want)
			}
		})
	}
}

func TestHitTestFullRegion_NoTabBar(t *testing.T) {
	t.Parallel()

	// When tabs are explicitly hidden the tab bar has height 0:
	//   y=[0..19]  content
	//   y=[20]     resize handle
	//   y=[21..25] editor (tabBarHeight = 0, editorHeight = 5)
	//   y>=26      context usage
	const (
		contentHeight = 20
		tabBarHeight  = 0
		editorHeight  = 5
	)

	tests := []struct {
		name string
		y    int
		want layoutRegion
	}{
		{name: "content", y: 10, want: regionContent},
		{name: "resize handle", y: 20, want: regionResizeHandle},
		{name: "editor first row", y: 21, want: regionEditor},
		{name: "editor last row", y: 25, want: regionEditor},
		{name: "context usage", y: 26, want: regionContextUsage},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := hitTestFullRegion(tt.y, contentHeight, tabBarHeight, editorHeight)
			if got != tt.want {
				t.Errorf("hitTestFullRegion(%d, %d, %d, %d) = %v, want %v",
					tt.y, contentHeight, tabBarHeight, editorHeight, got, tt.want)
			}
		})
	}
}

func TestFormatWindowTitle(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		appName      string
		sessionTitle string
		working      bool
		wantContains []string
		wantEquals   string
	}{
		{
			name:         "idle, no session title",
			appName:      "docker agent",
			sessionTitle: "",
			working:      false,
			wantEquals:   "docker agent",
		},
		{
			name:         "idle with session title",
			appName:      "docker agent",
			sessionTitle: "Refactor TUI",
			working:      false,
			wantEquals:   "Refactor TUI - docker agent",
		},
		{
			name:         "working prepends a spinner frame",
			appName:      "docker agent",
			sessionTitle: "",
			working:      true,
			// Spinner frame is a single rune followed by a space, then the
			// app name. We don't pin the exact rune (it depends on the
			// spinner package) but we do guarantee the suffix.
			wantContains: []string{" docker agent"},
		},
		{
			name:         "working with session title",
			appName:      "docker agent",
			sessionTitle: "Refactor TUI",
			working:      true,
			wantContains: []string{" Refactor TUI - docker agent"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := formatWindowTitle(0, tt.appName, tt.sessionTitle, tt.working)
			if tt.wantEquals != "" && got != tt.wantEquals {
				t.Errorf("formatWindowTitle = %q, want %q", got, tt.wantEquals)
			}
			for _, sub := range tt.wantContains {
				if !strings.Contains(got, sub) {
					t.Errorf("formatWindowTitle = %q, want to contain %q", got, sub)
				}
			}
		})
	}
}

func TestCommandCategories_DisabledCommandsFilter(t *testing.T) {
	t.Parallel()

	build := func(context.Context, tea.Model) []commands.Category {
		return []commands.Category{
			{
				Name: "Session",
				Commands: []commands.Item{
					{ID: "a", SlashCommand: "/cost"},
					{ID: "b", SlashCommand: "/eval"},
					{ID: "c", SlashCommand: "/exit"},
				},
			},
			{
				Name: "Settings",
				Commands: []commands.Item{
					{ID: "d", SlashCommand: "/theme"},
				},
			},
		}
	}

	t.Run("no filter keeps everything", func(t *testing.T) {
		t.Parallel()
		m := &appModel{ctx: t.Context, buildCommandCategories: build}
		got := m.commandCategories()
		if len(got) != 3 {
			t.Fatalf("len(categories) = %d, want 3 (Panes, Session, Settings)", len(got))
		}
		require.Equal(t, "Panes", got[0].Name)
		require.Len(t, got[0].Commands, 1)
		require.Equal(t, "/panes", got[0].Commands[0].SlashCommand)
	})

	t.Run("filters slash commands and drops empty categories", func(t *testing.T) {
		t.Parallel()
		m := &appModel{ctx: t.Context, buildCommandCategories: build}
		WithDisabledCommands([]string{"/cost", "eval", "/theme"})(m)

		got := m.commandCategories()
		if len(got) != 2 {
			t.Fatalf("len(categories) = %d, want 2 (Panes and Session kept, Settings dropped)", len(got))
		}
		require.Equal(t, "Panes", got[0].Name)
		require.Equal(t, "/panes", got[0].Commands[0].SlashCommand)
		got = got[1:]
		if got[0].Name != "Session" {
			t.Fatalf("category = %q, want Session", got[0].Name)
		}
		if len(got[0].Commands) != 1 || got[0].Commands[0].SlashCommand != "/exit" {
			t.Fatalf("session commands = %+v, want only /exit", got[0].Commands)
		}
	})

	t.Run("blank entries are ignored", func(t *testing.T) {
		t.Parallel()
		m := &appModel{ctx: t.Context, buildCommandCategories: build}
		WithDisabledCommands([]string{"", "  "})(m)
		got := m.commandCategories()
		if len(got) != 3 {
			t.Fatalf("len(categories) = %d, want 3 (Panes, Session, Settings)", len(got))
		}
		require.Equal(t, "Panes", got[0].Name)
		require.Len(t, got[0].Commands, 1)
		require.Equal(t, "/panes", got[0].Commands[0].SlashCommand)
	})

	t.Run("matching is case-insensitive", func(t *testing.T) {
		t.Parallel()
		m := &appModel{ctx: t.Context, buildCommandCategories: build}
		WithDisabledCommands([]string{"/Cost", "EVAL", "/Theme"})(m)
		got := m.commandCategories()
		if len(got) != 2 {
			t.Fatalf("len(categories) = %d, want 2 (Panes and Session kept, Settings dropped)", len(got))
		}
		require.Equal(t, "Panes", got[0].Name)
		require.Equal(t, "/panes", got[0].Commands[0].SlashCommand)
		got = got[1:]
		if got[0].Name != "Session" {
			t.Fatalf("category = %q, want Session", got[0].Name)
		}
		if len(got[0].Commands) != 1 || got[0].Commands[0].SlashCommand != "/exit" {
			t.Fatalf("session commands = %+v, want only /exit", got[0].Commands)
		}
	})

	t.Run("panes follows disabled commands and lean visibility", func(t *testing.T) {
		t.Parallel()
		m := &appModel{ctx: t.Context, buildCommandCategories: build}
		WithDisabledCommands([]string{"/PANES"})(m)
		got := m.commandCategories()
		require.Len(t, got, 2)
		require.Equal(t, "Session", got[0].Name)
		require.Equal(t, "Settings", got[1].Name)
		m.disabledCommands = nil
		m.leanMode = true
		got = m.commandCategories()
		require.Len(t, got, 2)
		require.Equal(t, "Session", got[0].Name)
		require.Equal(t, "Settings", got[1].Name)
		for _, category := range got {
			for _, item := range category.Commands {
				require.NotEqual(t, "/panes", item.SlashCommand)
			}
		}
	})
}

func newAttachedSubagentApp(ctx context.Context, sessions runtime.SessionRuntime, services app.Services, info runtime.SubagentAttachInfo, binding runtime.SessionBinding) *app.App {
	return app.New(ctx, sessions, info.Session, binding,
		app.WithRuntimeServices(services),
		app.WithSubagentAttach(info),
	)
}

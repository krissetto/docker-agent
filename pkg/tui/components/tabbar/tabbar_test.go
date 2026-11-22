package tabbar

import (
	"testing"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

func BenchmarkRunningTabsDraw(b *testing.B) {
	for _, tc := range []struct {
		name string
		drag bool
	}{
		{"Normal", false},
		{"Drag", true},
	} {
		b.Run(tc.name, func(b *testing.B) {
			runtimes := make([]*animation.Runtime, animation.Card.Len())
			for i := range runtimes {
				runtimes[i] = animation.NewSnapshotRuntime(time.Duration(i) * animation.Card.DefaultFrameDuration())
			}
			tabs := make([]messages.TabInfo, 12)
			for i := range tabs {
				tabs[i] = messages.TabInfo{Title: "Session", IsRunning: true, IsActive: i == 0}
			}
			bar := New(runtimes[0], 0)
			bar.SetWidth(200)
			bar.SetTabs(tabs, 0)
			if tc.drag {
				bar.drag = dragState{active: true, dragIdx: 1, dropIdx: 3}
			}
			for _, ar := range runtimes {
				bar.ar = ar
				_ = bar.View()
			}
			b.ReportAllocs()
			frame := 0
			for b.Loop() {
				bar.ar = runtimes[frame]
				_ = bar.View()
				frame = (frame + 1) % len(runtimes)
			}
		})
	}
}

func TestRunningTabFramesFollowThemeAndDragRoles(t *testing.T) {
	original := styles.CurrentTheme()
	t.Cleanup(func() { styles.ApplyTheme(original) })

	theme := *styles.DefaultTheme()
	styles.ApplyTheme(&theme)
	bar := New(animation.NewSnapshotRuntime(0), 0)
	info := messages.TabInfo{Title: "Running", IsRunning: true}
	checkFrames := func() []string {
		t.Helper()
		var views []string
		frames := animation.Card.Frames()
		for _, role := range []dragRole{dragRoleNone, dragRoleSource, dragRoleBystander, dragRoleNone} {
			bg := styles.TabBg
			fg := styles.EnsureContrast(styles.TabAccentFg, bg)
			if role == dragRoleBystander {
				fg = blendColors(fg, bg, dragBystanderDimAmount)
			}
			style := lipgloss.NewStyle().Foreground(fg).Background(bg)
			for i := range len(frames) + 1 {
				frame := frames[i%len(frames)]
				elapsed := time.Duration(i) * animation.Card.DefaultFrameDuration()
				tab := bar.renderTab(info, role, elapsed)
				require.Contains(t, tab.View(), style.Render(frame+" "))
				require.Equal(t, "▎"+frame+" Running × ", ansi.Strip(tab.View()))
				require.Equal(t, 10, tab.MainZoneEnd())
				require.Equal(t, 13, tab.Width())
				views = append(views, tab.View())
			}
		}
		return views
	}
	before := checkFrames()

	// A reload can change colors without changing even the theme pointer.
	theme.Colors.Highlight = "#ffeeaa"
	theme.Colors.TabBg = "#101011"
	styles.ApplyTheme(&theme)
	after := checkFrames()
	for i := range before {
		require.NotEqual(t, before[i], after[i])
	}

	info.NeedsAttention = true
	require.Equal(t, "▎! Running × ", ansi.Strip(bar.renderTab(info, dragRoleNone, 0).View()))
	info.NeedsAttention = false
	info.IsActive = true
	require.Equal(t, "▎ Running × ", ansi.Strip(bar.renderTab(info, dragRoleNone, 0).View()))
}

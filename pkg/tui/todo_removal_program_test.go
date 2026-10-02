package tui

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/messages"
)

func TestActualProgramTodoRemovalConfirmation(t *testing.T) {
	for _, entry := range []string{"sidebar", "inspector-mouse", "inspector-keyboard"} {
		for _, action := range []string{"confirm", "escape", "default-enter"} {
			t.Run(entry+"/"+action, func(t *testing.T) {
				root, handle := todoRemovalRoot(t)
				root.resizeAll()
				program := startTestProgram(t, root, &shellProgramModel{root: root}, tea.WithOutput(&cacheProgramWriter{}))
				if entry != "sidebar" {
					program.Send(messages.OpenTodosMsg{ID: "second"})
				} else {
					require.Eventually(t, func() bool {
						return strings.Contains(ansi.Strip(sidebarProgramSnapshot(t, program).content), "0/2 todos")
					}, time.Second, time.Millisecond)
					frame := sidebarProgramSnapshot(t, program)
					x, y := sidebarProgramPoint(t, frame.content, "0/2 todos")
					program.Send(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
				}
				require.Eventually(t, func() bool {
					s := sidebarProgramSnapshot(t, program)
					return s.active == 0 && strings.Contains(ansi.Strip(s.content), "Duplicate todo") && s.open == (entry != "sidebar")
				}, time.Second, time.Millisecond)
				if entry == "inspector-keyboard" {
					program.Send(tea.KeyPressMsg{Code: tea.KeyDelete})
				} else {
					frame := sidebarProgramSnapshot(t, program)
					x, y := -1, -1
					for row, line := range strings.Split(ansi.Strip(frame.content), "\n") {
						if prefix, _, found := strings.Cut(line, "Duplicate todo"); found {
							if entry == "inspector-mouse" && !strings.Contains(prefix, "×") {
								continue
							}
							x, y = ansi.StringWidth(prefix), row
						}
					}
					require.NotEqual(t, -1, x)
					program.Send(tea.MouseMotionMsg{X: x, Y: y})
					require.Eventually(t, func() bool {
						s := sidebarProgramSnapshot(t, program)
						return s.active == 0 && strings.Contains(strings.Split(ansi.Strip(s.content), "\n")[y], "×")
					}, time.Second, time.Millisecond)
					row := strings.Split(ansi.Strip(sidebarProgramSnapshot(t, program).content), "\n")[y]
					index := strings.LastIndex(row, "×")
					program.Send(tea.MouseClickMsg{X: ansi.StringWidth(row[:index]), Y: y, Button: tea.MouseLeft})
				}
				require.Eventually(t, func() bool {
					s := sidebarProgramSnapshot(t, program)
					return s.open && s.active == 0 && strings.Contains(ansi.Strip(s.content), "Remove todo") && strings.Contains(ansi.Strip(s.content), "Duplicate todo")
				}, time.Second, time.Millisecond)
				stored, err := handle.Todos(t.Context())
				require.NoError(t, err)
				require.Len(t, stored, 2, "first click only opens confirmation")
				switch action {
				case "escape":
					program.Send(tea.KeyPressMsg{Code: tea.KeyEscape})
				case "default-enter":
					program.Send(tea.KeyPressMsg{Code: tea.KeyEnter})
				case "confirm":
					frame := sidebarProgramSnapshot(t, program)
					x, y := -1, -1
					for row, line := range strings.Split(ansi.Strip(frame.content), "\n") {
						if strings.Contains(line, "Cancel") {
							if prefix, _, found := strings.Cut(line, "Remove"); found {
								x, y = ansi.StringWidth(prefix), row
							}
						}
					}
					require.NotEqual(t, -1, x)
					program.Send(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
				}
				require.Eventually(t, func() bool {
					s := sidebarProgramSnapshot(t, program)
					return s.active == 0 && !strings.Contains(ansi.Strip(s.content), "Remove todo")
				}, time.Second, time.Millisecond)
				stored, err = handle.Todos(t.Context())
				require.NoError(t, err)
				if action == "confirm" {
					require.Len(t, stored, 1)
					require.Equal(t, "first", stored[0].ID, "duplicate descriptions do not identify removal targets")
				} else {
					require.Len(t, stored, 2)
				}
				if entry != "sidebar" {
					require.True(t, sidebarProgramSnapshot(t, program).open, "confirmation closes only its own layer")
					require.NotContains(t, ansi.Strip(sidebarProgramSnapshot(t, program).content), "Saving…")
				}
			})
		}
	}
}

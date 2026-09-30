package tui

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"charm.land/lipgloss/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/components/completion"
	"github.com/docker/docker-agent/pkg/tui/components/messagebar"
	"github.com/docker/docker-agent/pkg/tui/dialog"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/page/chat"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

func TestRootBackgroundAllThemesAndLiveSwitch(t *testing.T) {
	setupAutoThemeTest(t)
	refs, err := styles.ListThemeRefs()
	require.NoError(t, err)
	for _, mode := range []struct{ lean, sidebar bool }{{}, {sidebar: true}, {lean: true}} {
		lean := mode.lean
		root, _, _ := frozenClockRoot(t, 120, 40)
		root.leanMode = lean
		root.hideSidebar = !mode.sidebar
		if lean || mode.sidebar {
			root.initSessionComponents("profile", root.application, root.application.Session())
			root.chatPage.Init()
		}
		root.messageBar = messagebar.New()
		root.resizeAll()
		for _, ref := range refs {
			if !styles.IsBuiltinTheme(ref) {
				continue
			}
			t.Run(fmt.Sprintf("%s/lean=%v/sidebar=%v", ref, lean, mode.sidebar), func(t *testing.T) {
				theme, err := styles.LoadTheme(ref)
				require.NoError(t, err)
				styles.ApplyTheme(theme)
				require.Equal(t, styles.Background, root.View().BackgroundColor, "theme generation invalidates cached terminal default")
				require.Nil(t, chromeCells(root.View().Content)[0].bg)
				_, _ = root.Update(messages.ThemeChangedMsg{})
				for _, popup := range []bool{false, true} {
					if popup {
						root.updateCompletionsCmd(completion.OpenMsg{Items: []completion.Item{{Label: "Candidate", Value: "candidate"}}})
					} else {
						root.updateCompletionsCmd(completion.CloseMsg{})
					}
					root.viewCacheValid = false
					view := root.View()
					require.Equal(t, styles.Background, view.BackgroundColor)
					require.Equal(t, !lean, view.AltScreen)
					rows := strings.Split(view.Content, "\n")
					frame := chromeCells(view.Content)
					require.Len(t, rows, 40)
					require.Len(t, frame, 120*40)
					for _, row := range rows {
						require.Equal(t, 120, ansi.StringWidth(row))
					}
					for y := range rows {
						cells := frame[y*120 : (y+1)*120]
						require.Len(t, cells, 120)
						require.Nil(t, cells[0].bg, "left margin row %d", y)
						require.Nil(t, cells[119].bg, "right margin row %d", y)
					}
					require.Nil(t, frame[60].bg, "page canvas")
					if mode.sidebar && !popup {
						geometry := root.chatPage.(chat.SplitPresentation).MeasureSplitShell(root.width, root.contentHeight).Sidebar
						require.Positive(t, geometry.Width)
						require.Positive(t, geometry.Height)
						for y := geometry.Y; y < geometry.Y+geometry.Height; y++ {
							for _, x := range []int{geometry.X, geometry.X + geometry.Width - 1} {
								require.Nil(t, frame[y*120+x].bg, "sidebar canvas (%d,%d)", x, y)
							}
						}
					}
					footer := layoutTerminalCells(root.renderMessageBar())[0]
					composedFooter := layoutTerminalCells(view.Content)[39]
					for x, cell := range footer {
						require.Equal(t, cell, composedFooter[x], "footer retains only its intentional local surfaces")
					}
					if !lean {
						_, editorHeight := root.editor.GetSize()
						contextY := root.editorTop() + editorHeight
						cells := frame[contextY*120 : (contextY+1)*120]
						label := " Context 0%"
						before, _, found := strings.Cut(ansi.Strip(rows[contextY]), label)
						require.True(t, found)
						for x := ansi.StringWidth(before); x < 120; x++ {
							require.Nil(t, cells[x].bg, "context label/cutout")
						}
						require.Equal(t, styles.Background, cells[styles.EditorHMargin].fg, "empty lower half agrees with canvas")
						require.Equal(t, styles.EditorBg, cells[styles.EditorHMargin].bg, "upper half retains editor surface")
						require.Equal(t, styles.EditorBg, frame[(contextY-1)*120+60].bg)
						if ref == "gruvbox-dark" {
							require.NotEqual(t, styles.Background, styles.EditorBg)
						}
					}
					require.Equal(t, view.Content, root.View().Content, "default-background canvas participates in root view cache")
				}
				root.messageBar.SetMessage(messagebar.Message{Text: "Notice", Actions: []messagebar.Action{{Label: "Act"}}})
				root.messageBar.SetFocused(true)
				root.viewCacheValid = false
				local := layoutTerminalCells(root.renderMessageBar())[0]
				painted := layoutTerminalCells(root.View().Content)[39]
				for x, cell := range local {
					require.Equal(t, cell, painted[x], "action background and attributes cell %d", x)
				}
				require.Equal(t, styles.Background, painted[styles.AppPadding].Style.Bg, "notice retains intentional messagebar surface")
				require.Equal(t, styles.Background, painted[118].Style.Bg, "notice trailing space retains messagebar surface")
				root.messageBar.SetMessage(messagebar.Message{})
			})
		}
	}
}

func TestRootBackgroundPreservesNestedStylesAndEscapeBytes(t *testing.T) {
	setupAutoThemeTest(t)
	theme, err := styles.LoadTheme("gruvbox-dark")
	require.NoError(t, err)
	styles.ApplyTheme(theme)
	for _, sequence := range []string{
		"\x1b[m", "\x1b[0m", "\x1b[49m", "\x1b[0;1;39m", "\x1b[49;3;38;2;4;5;6m",
		"\x1b[0;48;2;12;34;56m", "\x1b[48:2::12:34:56m", "\x1b[48;5;123m", "\x1b[104m",
	} {
		content := "default " + lipgloss.NewStyle().Background(styles.EditorBg).Render("editor") +
			lipgloss.NewStyle().Background(styles.CardBg).Bold(true).Render("card") +
			lipgloss.NewStyle().Background(lipgloss.Color(*styles.MarkdownStyle().Code.BackgroundColor)).Render("code") +
			styles.DiffAddStyle.Render("added") + styles.DiffRemoveStyle.Render("removed") + sequence + " tail"
		before := uv.NewStyledString(content).Lines(ansi.GraphemeWidth)
		after := uv.NewStyledString(toFullscreenView(content, "title", false, false).Content).Lines(ansi.GraphemeWidth)
		require.Len(t, after, len(before))
		for y, line := range before {
			require.Len(t, after[y], len(line))
			for x, cell := range line {
				require.Equal(t, cell, after[y][x], "sequence %q cell %d", sequence, x)
			}
		}
	}
	for _, opaque := range []string{
		"\x1b]8;;https://example.invalid\x1b\\linked\x1b]8;;\x1b\\",
		"\x1b_Ga=T,f=100;AAAA\x1b\\",
		"\x1b]1337;File=inline=1:AAAA\a",
		"\x1bPq~?~?\x1b\\",
	} {
		content := "e\x1b[7m\x1b[27m\u0301 界 👩‍💻" + opaque
		painted := toFullscreenView(content, "title", false, false).Content
		require.Contains(t, painted, opaque)
		require.Contains(t, painted, "e\x1b[7m\x1b[27m\u0301 界 👩‍💻")
		require.Equal(t, ansi.Strip(content), ansi.Strip(painted))
	}
}

func TestRootBackgroundLoadingAndError(t *testing.T) {
	setupAutoThemeTest(t)
	root, _, _ := frozenClockRoot(t, 40, 10)
	for _, lean := range []bool{false, true} {
		root.leanMode = lean
		for _, failure := range []error{nil, errors.New("failed to load")} {
			root.ready, root.err = false, failure
			root.viewCacheValid = false
			content := root.View().Content
			rows := strings.Split(content, "\n")
			require.Len(t, rows, 10)
			cells := chromeCells(content)
			require.Len(t, cells, 400)
			for _, cell := range cells {
				require.Nil(t, cell.bg)
			}
		}
	}
}

func TestRootDialogDefaultBackgroundSurvivesThemeSwitch(t *testing.T) {
	setupAutoThemeTest(t)
	for _, ref := range []string{"gruvbox-dark", "nord", "default"} {
		theme, err := styles.LoadTheme(ref)
		require.NoError(t, err)
		styles.ApplyTheme(theme)
		d := dialog.NewExitConfirmationDialog()
		d.SetSize(80, 24)
		row, col := d.Position()
		content := composeRootLayers([]*lipgloss.Layer{lipgloss.NewLayer(d.View()).X(col).Y(row)}, 80, 24)
		content = paneClipped(content, 80, 24)
		view := toFullscreenView(content, "test", false, false)
		cells := layoutTerminalCells(view.Content)
		for y, line := range strings.Split(ansi.Strip(view.Content), "\n") {
			if strings.Contains(line, "Exit") || strings.Contains(line, "Do you want to exit?") {
				for x, cell := range cells[y] {
					require.Nil(t, cell.Style.Bg, "dialog title/question/padding remains default at %d,%d", x, y)
				}
			}
			if before, _, found := strings.Cut(line, "No ↵"); found {
				require.Equal(t, styles.Selected, cells[y][ansi.StringWidth(before)].Style.Bg, "selected button remains explicit")
			}
		}
		require.Nil(t, cells[0][0].Style.Bg, "outer canvas remains terminal default")
		require.Equal(t, content, view.Content, "fullscreen adapter preserves exact ANSI/APC/grapheme bytes")
		require.Equal(t, styles.Background, view.BackgroundColor, "existing OSC 11 behavior retained")
	}
}

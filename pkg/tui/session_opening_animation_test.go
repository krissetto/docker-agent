package tui

import (
	"fmt"
	"image/color"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

// Deliver exact elapsed durations through the production tick lease boundary.
// No acquisition, replay, editor IO or wall-clock commands are executed.
type openingFadeScheduler struct {
	now  time.Time
	step time.Duration
}

func (s *openingFadeScheduler) Now() time.Time { return s.now }
func (s *openingFadeScheduler) Tick(_ time.Duration, create func(time.Time) tea.Msg) tea.Cmd {
	return func() tea.Msg {
		s.now = s.now.Add(s.step)
		return create(s.now)
	}
}

func openingFadeRoot(t *testing.T) (*appModel, *openingFadeScheduler) {
	t.Helper()
	s := &openingFadeScheduler{now: time.Unix(1, 0)}
	root, _, _ := harnessRoot(t, 120, 40, animation.NewRuntimeWithScheduler(s))
	root.beginSubagentOpening("child-node", "target", "Worker", "root")
	t.Cleanup(root.cancelSessionOpening)
	return root, s
}

func openingFadeStep(t *testing.T, root *appModel, s *openingFadeScheduler, elapsed time.Duration) {
	t.Helper()
	s.step = elapsed
	cmd := root.ar.Continue()
	require.NotNil(t, cmd)
	tick, ok := root.ar.Accept(cmd().(animation.TickMsg))
	require.True(t, ok)
	root.tickSessionOpening(tick)
	require.True(t, tick.Dirty())
}

func openingFadeColor(c color.Color, alpha float64) color.Color {
	if c == nil {
		c = styles.TextPrimary
	}
	r, g, b, _ := c.RGBA()
	br, bg, bb, _ := styles.Background.RGBA()
	return color.RGBA{
		R: uint8(float64(br>>8) + alpha*(float64(r>>8)-float64(br>>8))),
		G: uint8(float64(bg>>8) + alpha*(float64(g>>8)-float64(bg>>8))),
		B: uint8(float64(bb>>8) + alpha*(float64(b>>8)-float64(bb>>8))),
		A: 255,
	}
}

func assertOpeningFadeCells(t *testing.T, original, faded string, alpha float64) {
	t.Helper()
	require.Equal(t, ansi.Strip(original), ansi.Strip(faded), "fade preserves glyphs and geometry")
	before, after := chromeCells(original), chromeCells(faded)
	require.Len(t, after, len(before))
	for i, cell := range before {
		if strings.TrimSpace(cell.glyph) != "" {
			require.Equal(t, openingFadeColor(cell.fg, alpha), after[i].fg, "foreground cell %d (%s)", i, cell.glyph)
		}
		if cell.bg == nil {
			require.Nil(t, after[i].bg, "terminal-default background cell %d", i)
		} else {
			require.Equal(t, openingFadeColor(cell.bg, alpha), after[i].bg, "explicit surface cell %d", i)
		}
	}
}

func TestOpeningLoaderFadeFromFirstFrameAndPreservedFadeOut(t *testing.T) {
	root, scheduler := openingFadeRoot(t)
	r := root.opening
	assertLoader := func(alpha float64) {
		t.Helper()
		original := paneClipped(lipgloss.Place(root.width, root.contentHeight, lipgloss.Center, lipgloss.Center, r.spinner.View()), root.width, root.contentHeight)
		actual := root.openingContent()
		require.Contains(t, ansi.Strip(actual), "Loading...")
		require.Contains(t, ansi.Strip(actual), r.spinner.RawFrame())
		assertOpeningFadeCells(t, original, actual, alpha)
		if alpha == 1 {
			require.Equal(t, original, actual, "fully visible loader is unchanged")
		}
	}
	assertLoader(0)
	openingFadeStep(t, root, scheduler, animation.LoadingMinDuration/2)
	assertLoader(animation.EaseOutCubic(0.5))
	openingFadeStep(t, root, scheduler, animation.LoadingMinDuration/2)
	assertLoader(1)
	require.Equal(t, openingWaiting, r.phase, "slow acquisition keeps the full loader visible")
	r.installed = true
	openingFadeStep(t, root, scheduler, animation.TickRate)
	require.Equal(t, openingLoaderOut, r.phase)
	assertLoader(1)
	openingFadeStep(t, root, scheduler, animation.ShortDuration/2)
	assertLoader(0.5)
	openingFadeStep(t, root, scheduler, animation.ShortDuration/2)
	require.Equal(t, openingChatIn, r.phase)
	require.Zero(t, r.transition.Value())
	require.NotContains(t, ansi.Strip(root.openingContent()), "Loading...")
}

func TestOpeningKeepsComposerAndPanelVisibleThroughEveryPhase(t *testing.T) {
	for _, lean := range []bool{false, true} {
		for _, width := range []int{40, 120} {
			t.Run(fmt.Sprintf("lean=%v/width=%d", lean, width), func(t *testing.T) {
				root, scheduler := openingFadeRoot(t)
				root.leanMode = lean
				if lean {
					root.initSessionComponents("profile", root.application, root.application.Session())
					root.chatPage.Init()
					root.editor.Blur()
				}
				root.application.Session().WorkingDir = "/workspace/project"
				root.applyPanelSettings(messages.DefaultPanelSettings())
				root.editor.SetValue("COMPOSER é 界 👩‍💻\nsecond draft line\nthird draft line")
				attachment := filepath.Join(t.TempDir(), "note.txt")
				require.NoError(t, os.WriteFile(attachment, []byte("attachment"), 0o600))
				require.NoError(t, root.editor.AttachFile(attachment))
				draft := root.editor.Value()
				root.handleWindowResize(width, 40)
				r := root.opening
				root.opening = nil
				original := root.composeView().Content
				layout, focus := root.composerLayout(), root.paneFocus()
				root.opening = r
				require.Contains(t, ansi.Strip(original), "COMPOSER")
				require.Contains(t, ansi.Strip(original), "third draft")
				require.Contains(t, ansi.Strip(original), "note.txt")
				rows := strings.Split(original, "\n")
				require.Contains(t, ansi.Strip(rows[39]), "project")
				assertScene := func() {
					t.Helper()
					actual := root.composeView().Content
					actualRows := strings.Split(actual, "\n")
					require.Len(t, actualRows, 40)
					for _, row := range actualRows {
						require.Equal(t, width, ansi.StringWidth(row))
					}
					// Compare terminal cells, not redundant ANSI resets inserted at
					// the boundary between the faded transcript and the live shell.
					require.Equal(t, chromeCells(strings.Join(rows[layout.bannerTop:], "\n")), chromeCells(strings.Join(actualRows[layout.bannerTop:], "\n")), "composer, attachments, context and bottom panel remain fully visible")
					require.Equal(t, layout, root.composerLayout())
					require.Equal(t, focus, root.paneFocus())
					for _, msg := range []tea.Msg{tea.KeyPressMsg{Code: tea.KeyEnter}, messages.SendMsg{Content: "blocked"}} {
						_, cmd := root.update(msg)
						require.Nil(t, cmd, "opening must not dispatch a send")
						require.Equal(t, draft, root.editor.Value())
					}
				}
				assertScene() // first loader frame, alpha 0
				openingFadeStep(t, root, scheduler, animation.LoadingMinDuration/2)
				assertScene()
				openingFadeStep(t, root, scheduler, animation.LoadingMinDuration/2)
				assertScene() // waiting on acquisition, full loader
				r.installed = true
				openingFadeStep(t, root, scheduler, animation.TickRate)
				require.Equal(t, openingLoaderOut, r.phase)
				assertScene()
				openingFadeStep(t, root, scheduler, animation.ShortDuration/2)
				assertScene()
				openingFadeStep(t, root, scheduler, animation.ShortDuration/2)
				require.Equal(t, openingChatIn, r.phase)
				assertScene() // chat alpha 0 must not hide the input
				transcript := strings.Join(rows[:layout.bannerTop], "\n")
				assertOpeningFadeCells(t, transcript, strings.Join(strings.Split(root.composeView().Content, "\n")[:layout.bannerTop], "\n"), 0)
				openingFadeStep(t, root, scheduler, animation.MediumDuration/2)
				assertScene()
				assertOpeningFadeCells(t, transcript, strings.Join(strings.Split(root.composeView().Content, "\n")[:layout.bannerTop], "\n"), animation.EaseOutCubic(0.5))
				openingFadeStep(t, root, scheduler, animation.MediumDuration/2)
				require.Nil(t, root.opening)
				require.Equal(t, layout, root.composerLayout())
				require.Equal(t, focus, root.paneFocus())
				require.Equal(t, chromeCells(rows[39]), chromeCells(strings.Split(root.composeView().Content, "\n")[39]))
				_, cmd := root.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
				require.Empty(t, root.editor.Value(), "Enter resumes normal send behavior after opening")
				var sent []messages.SendMsg
				for _, msg := range collectMsgs(cmd) {
					if routed, ok := msg.(messages.RoutedMsg); ok {
						msg = routed.Inner
					}
					if send, ok := msg.(messages.SendMsg); ok {
						sent = append(sent, send)
					}
				}
				require.Len(t, sent, 1)
				require.Equal(t, draft, sent[0].Content)
			})
		}
	}
}

func TestOpeningFadePreservesOpaqueEscapesAndGraphemes(t *testing.T) {
	for _, opaque := range []string{
		"\x1b]8;;https://example.invalid\x1b\\linked\x1b]8;;\x1b\\",
		"\x1b_Ga=T,f=100;AAAA\x1b\\",
		"\x1b]1337;File=inline=1:AAAA\a",
		"\x1bPq~?~?\x1b\\",
	} {
		content := "é 界 👩‍💻 " + opaque
		faded := fadeOpeningContent(content, 0.5)
		require.Contains(t, faded, opaque)
		require.Equal(t, ansi.Strip(content), ansi.Strip(faded))
		require.Equal(t, content, fadeOpeningContent(content, 1))
	}
}

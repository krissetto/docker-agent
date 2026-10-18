package notification

import (
	"fmt"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/styles"
)

func TestNotification_InitialState(t *testing.T) {
	t.Parallel()

	n := New()

	require.Empty(t, n.items)
	require.False(t, n.Open())
}

func TestNotification_AutoHideDurations(t *testing.T) {
	t.Parallel()

	require.Equal(t, defaultDuration, New().autoHideDuration)
}

func TestNotification_Show(t *testing.T) {
	t.Parallel()

	n := New()

	updated, cmd := n.Update(ShowMsg{Text: "Test notification"})

	require.Len(t, updated.items, 1)
	require.Equal(t, "Test notification", updated.items[0].Text)
	require.Equal(t, TypeSuccess, updated.items[0].Type)
	require.True(t, updated.Open())
	require.NotEmpty(t, updated.View())
	require.NotNil(t, cmd)
}

func TestNotification_Hide(t *testing.T) {
	t.Parallel()

	n := New()

	updated, _ := n.Update(ShowMsg{Text: "Test"})
	require.Len(t, updated.items, 1)

	updated, _ = updated.Update(HideMsg{})

	require.Empty(t, updated.items)
	require.False(t, updated.Open())
	require.Empty(t, updated.View())
}

func TestNotification_HideByID(t *testing.T) {
	t.Parallel()

	n := New()
	updated, _ := n.Update(ShowMsg{Text: "first"})
	updated, _ = updated.Update(ShowMsg{Text: "second"})
	require.Len(t, updated.items, 2)

	firstID := updated.items[1].ID
	updated, _ = updated.Update(HideMsg{ID: firstID})

	require.Len(t, updated.items, 1)
	require.Equal(t, "second", updated.items[0].Text)
}

func TestNotification_DismissByID(t *testing.T) {
	t.Parallel()

	n := New()
	updated, _ := n.Update(ShowMsg{Text: "dismiss me", Type: TypeWarning})
	id := updated.items[0].ID

	updated, _ = updated.Update(DismissMsg{ID: id})

	require.Empty(t, updated.items)
	require.False(t, updated.Open())
}

func TestNotification_Position(t *testing.T) {
	t.Parallel()

	n := New()
	n.SetSize(100, 50)
	updated, _ := n.Update(ShowMsg{Text: "Test"})
	row, col := updated.position()

	view := updated.View()
	require.Equal(t, 50-lipgloss.Height(view)-notificationPadding, row)
	require.Equal(t, 100-lipgloss.Width(view)-notificationPadding, col)
}

func TestNotification_GetLayers(t *testing.T) {
	t.Parallel()

	n := New()

	require.Empty(t, n.GetLayers())
	n.SetSize(100, 50)

	updated, _ := n.Update(ShowMsg{Text: "Test"})
	require.NotEmpty(t, updated.GetLayers())
}

func TestNotification_MixedWidthLayersMatchHitGeometry(t *testing.T) {
	t.Parallel()

	for _, size := range [][2]int{{100, 50}, {24, 50}, {8, 4}} {
		t.Run(fmt.Sprintf("%dx%d", size[0], size[1]), func(t *testing.T) {
			n := New()
			n.SetSize(size[0], size[1])
			for _, text := range []string{strings.Repeat("wide 界 ", 20), "x", "another long notification"} {
				n.Update(ShowMsg{Text: text})
			}
			layers, bounds := n.GetLayers(), n.itemBounds()
			require.Len(t, layers, 3)
			require.Less(t, bounds[1].width, bounds[0].width)
			for i, b := range bounds {
				layer := layers[i]
				require.Equal(t, b.col, layer.GetX())
				require.Equal(t, b.row, layer.GetY())
				require.Equal(t, b.width, layer.Width())
				require.Equal(t, b.height, layer.Height())
				require.Equal(t, bounds[0].col+bounds[0].width, b.col+b.width)
				if !n.onScreen(b.col, b.row) {
					continue
				}
				id, text, ok := n.BodyHit(b.col, b.row)
				require.True(t, ok)
				require.Equal(t, b.id, id)
				require.Equal(t, b.text, text)
				n.HandleMouseMotion(b.col, b.row)
				id, _, ok = n.CopyHit(b.col, b.row)
				require.True(t, ok)
				require.Equal(t, b.id, id)
				x, y := closeButtonPosition(b)
				if n.onScreen(x, y) {
					command := n.HandleClick(x, y)
					require.NotNil(t, command)
					require.Equal(t, DismissMsg{ID: b.id}, command())
				}
			}
			short := bounds[1]
			for _, point := range [][2]int{{short.col - 1, short.row}, {size[0] - 1, short.row}, {size[0] - 3, bounds[2].row + bounds[2].height}} {
				x, y := point[0], point[1]
				_, _, hit := n.BodyHit(x, y)
				require.False(t, hit)
				require.Nil(t, n.HandleClick(x, y))
				n.HandleMouseMotion(x, y)
				require.Zero(t, n.hoveredID)
			}
		})
	}
}

func TestNotification_AutoHideGeneration(t *testing.T) {
	t.Parallel()

	n := New()
	updated, _ := n.Update(ShowMsg{Text: "auto", Type: TypeInfo})
	id := updated.items[0].ID
	require.Equal(t, uint64(0), updated.items[0].timerGen)

	updated, _ = updated.Update(AutoHideMsg{ID: id, Generation: 0})

	require.Empty(t, updated.items)
}

func TestNotification_StaleTimerIgnored(t *testing.T) {
	t.Parallel()

	n := New()
	n.SetSize(100, 50)
	updated, _ := n.Update(ShowMsg{Text: "hover me", Type: TypeInfo})
	id := updated.items[0].ID
	row, col := updated.position()

	updated, cmd := updated.HandleMouseMotion(col+1, row+1)
	require.Nil(t, cmd)
	require.Equal(t, uint64(1), updated.items[0].timerGen)

	updated, _ = updated.Update(AutoHideMsg{ID: id, Generation: 0})

	require.Len(t, updated.items, 1)
	require.Equal(t, id, updated.items[0].ID)
}

func TestNotification_HoverEnterLeaveRestartsTimer(t *testing.T) {
	t.Parallel()

	n := New()
	// Zero duration so the auto-hide timer resolves promptly; set on this
	// Manager only, so the test stays parallel-safe.
	n.autoHideDuration = 0
	n.SetSize(100, 50)
	updated, _ := n.Update(ShowMsg{Text: "hover", Type: TypeInfo})
	id := updated.items[0].ID
	row, col := updated.position()

	updated, cmd := updated.HandleMouseMotion(col+1, row+1)
	require.Nil(t, cmd)
	require.Equal(t, id, updated.hoveredID)
	require.Equal(t, uint64(1), updated.items[0].timerGen)

	updated, cmd = updated.HandleMouseMotion(0, 0)
	require.NotNil(t, cmd)
	require.Zero(t, updated.hoveredID)

	msg := cmd()
	auto, ok := msg.(AutoHideMsg)
	require.True(t, ok)
	require.Equal(t, id, auto.ID)
	require.Equal(t, uint64(1), auto.Generation)
}

func TestNotification_WarningHoverEnterLeaveRestartsTimer(t *testing.T) {
	t.Parallel()

	n := New()
	// Zero duration so the auto-hide timer resolves promptly; set on this
	// Manager only, so the test stays parallel-safe.
	n.autoHideDuration = 0
	n.SetSize(100, 50)
	updated, showCmd := n.Update(ShowMsg{Text: "warning", Type: TypeWarning})
	require.NotNil(t, showCmd)
	id := updated.items[0].ID
	row, col := updated.position()

	updated, cmd := updated.HandleMouseMotion(col+1, row+1)
	require.Nil(t, cmd)
	require.Equal(t, id, updated.hoveredID)
	require.Equal(t, uint64(1), updated.items[0].timerGen)

	updated, cmd = updated.HandleMouseMotion(0, 0)
	require.NotNil(t, cmd)
	require.Zero(t, updated.hoveredID)

	msg := cmd()
	auto, ok := msg.(AutoHideMsg)
	require.True(t, ok)
	require.Equal(t, id, auto.ID)
	require.Equal(t, uint64(1), auto.Generation)
}

func TestNotification_CloseButtonHit(t *testing.T) {
	t.Parallel()

	n := New()
	n.SetSize(100, 50)
	updated, _ := n.Update(ShowMsg{Text: "close", Type: TypeWarning})
	id := updated.items[0].ID
	x, y := closeButtonCoords(t, updated, id)

	hitID, ok := updated.CloseButtonHit(x, y)
	require.True(t, ok)
	require.Equal(t, id, hitID)
}

func TestNotification_CloseButtonHitWithWideText(t *testing.T) {
	t.Parallel()

	n := New()
	n.SetSize(100, 50)
	updated, _ := n.Update(ShowMsg{Text: "wide 🚀 漢字", Type: TypeInfo})
	id := updated.items[0].ID
	x, y := closeButtonCoords(t, updated, id)

	hitID, ok := updated.CloseButtonHit(x, y)
	require.True(t, ok)
	require.Equal(t, id, hitID)
	plainLine := strings.Split(ansi.Strip(updated.View()), "\n")[1]
	require.Contains(t, plainLine, closeGlyph)
}

func TestNotification_HandleClickDismissesOnlyCloseButton(t *testing.T) {
	t.Parallel()

	n := New()
	n.SetSize(100, 50)
	updated, _ := n.Update(ShowMsg{Text: "click", Type: TypeWarning})
	id := updated.items[0].ID
	x, y := closeButtonCoords(t, updated, id)

	bodyCmd := updated.HandleClick(x-2, y)
	require.Nil(t, bodyCmd)

	cmd := updated.HandleClick(x, y)
	require.NotNil(t, cmd)
	msg := cmd()
	dismiss, ok := msg.(DismissMsg)
	require.True(t, ok)
	require.Equal(t, id, dismiss.ID)

	updated, _ = updated.Update(dismiss)
	require.Empty(t, updated.items)
}

func TestNotification_StackingLayerBasics(t *testing.T) {
	t.Parallel()

	n := New()
	n.SetSize(100, 50)
	updated, _ := n.Update(ShowMsg{Text: "old"})
	updated, _ = updated.Update(ShowMsg{Text: "new"})

	view := ansi.Strip(updated.View())
	require.Contains(t, view, "old")
	require.Contains(t, view, "new")
	require.Less(t, stringsIndex(t, view, "old"), stringsIndex(t, view, "new"))
	require.NotEmpty(t, updated.GetLayers())
	require.Len(t, updated.itemBounds(), 2)
	bounds := updated.itemBounds()
	require.Equal(t, 50-lipgloss.Height(updated.View())-notificationPadding, bounds[0].row)
	require.Less(t, bounds[0].row, bounds[1].row)
}

func TestNotification_CloseGlyphAndPaddingRegression(t *testing.T) {
	t.Parallel()

	n := New()
	updated, _ := n.Update(ShowMsg{Text: "glyph"})
	plain := ansi.Strip(updated.View())

	require.Contains(t, plain, closeGlyph)
	require.NotContains(t, plain, "[x]")
	require.Equal(t, 3, styles.NotificationStyle.GetPaddingRight())
	require.Equal(t, 3, styles.NotificationInfoStyle.GetPaddingRight())
	require.Equal(t, 3, styles.NotificationWarningStyle.GetPaddingRight())
	require.Equal(t, 3, styles.NotificationErrorStyle.GetPaddingRight())
}

func TestNotification_WarningsAutoHide(t *testing.T) {
	t.Parallel()

	n := New()
	// Zero duration so the auto-hide timer resolves promptly; set on this
	// Manager only, so the test stays parallel-safe.
	n.autoHideDuration = 0
	updated, cmd := n.Update(ShowMsg{Text: "warn", Type: TypeWarning})
	require.NotNil(t, cmd)
	require.Len(t, updated.items, 1)

	warnID := updated.items[0].ID
	msg := cmd()
	auto, ok := msg.(AutoHideMsg)
	require.True(t, ok)
	require.Equal(t, warnID, auto.ID)
	require.Equal(t, uint64(0), auto.Generation)

	updated, _ = updated.Update(auto)
	require.Empty(t, updated.items)
}

func TestNotification_ErrorAutoHides(t *testing.T) {
	t.Parallel()

	n := New()
	// Zero duration so the auto-hide timer resolves promptly; set on this
	// Manager only, so the test stays parallel-safe.
	n.autoHideDuration = 0
	updated, cmd := n.Update(ShowMsg{Text: "err", Type: TypeError})
	require.NotNil(t, cmd)
	require.Len(t, updated.items, 1)

	errID := updated.items[0].ID
	msg := cmd()
	auto, ok := msg.(AutoHideMsg)
	require.True(t, ok)
	require.Equal(t, errID, auto.ID)
	require.Equal(t, uint64(0), auto.Generation)

	updated, _ = updated.Update(auto)
	require.Empty(t, updated.items)
}

func TestNotification_ErrorHoverEnterLeaveRestartsTimer(t *testing.T) {
	t.Parallel()

	n := New()
	// Zero duration so the auto-hide timer resolves promptly; set on this
	// Manager only, so the test stays parallel-safe.
	n.autoHideDuration = 0
	n.SetSize(100, 50)
	updated, _ := n.Update(ShowMsg{Text: "err", Type: TypeError})
	id := updated.items[0].ID
	row, col := updated.position()

	updated, cmd := updated.HandleMouseMotion(col+1, row+1)
	require.Nil(t, cmd)
	require.Equal(t, id, updated.hoveredID)
	require.Equal(t, uint64(1), updated.items[0].timerGen)

	updated, _ = updated.Update(AutoHideMsg{ID: id, Generation: 0})
	require.Len(t, updated.items, 1)

	updated, cmd = updated.HandleMouseMotion(0, 0)
	require.NotNil(t, cmd)
	require.Zero(t, updated.hoveredID)

	msg := cmd()
	auto, ok := msg.(AutoHideMsg)
	require.True(t, ok)
	require.Equal(t, id, auto.ID)
	require.Equal(t, uint64(1), auto.Generation)
}

func TestNotification_ErrorAutodetectAutoHides(t *testing.T) {
	t.Parallel()

	n := New()
	// Zero duration so the auto-hide timer resolves promptly; set on this
	// Manager only, so the test stays parallel-safe.
	n.autoHideDuration = 0
	updated, cmd := n.Update(ShowMsg{Text: "operation failed"})

	require.NotNil(t, cmd)
	require.Equal(t, TypeError, updated.items[0].Type)
}

func TestNotification_BodyHit(t *testing.T) {
	t.Parallel()

	n := New()
	n.SetSize(100, 50)
	updated, _ := n.Update(ShowMsg{Text: "copy this", Type: TypeInfo})
	id := updated.items[0].ID
	bounds := updated.itemBounds()[0]

	hitID, text, ok := updated.BodyHit(bounds.col+1, bounds.row+1)
	require.True(t, ok)
	require.Equal(t, id, hitID)
	require.Equal(t, "copy this", text)

	closeX, closeY := closeButtonCoords(t, updated, id)
	hitID, text, ok = updated.BodyHit(closeX, closeY)
	require.False(t, ok)
	require.Zero(t, hitID)
	require.Empty(t, text)
}

func TestNotification_CopyHitRequiresHoveredBody(t *testing.T) {
	t.Parallel()

	n := New()
	n.SetSize(120, 50)
	updated, _ := n.Update(ShowMsg{Text: "copy this", Type: TypeInfo})
	id := updated.items[0].ID
	bounds := updated.itemBounds()[0]
	bodyX, bodyY := bounds.col+1, bounds.row+1

	hitID, text, ok := updated.CopyHit(bodyX, bodyY)
	require.False(t, ok)
	require.Zero(t, hitID)
	require.Empty(t, text)

	updated, _ = updated.HandleMouseMotion(bodyX, bodyY)
	hitID, text, ok = updated.CopyHit(bodyX, bodyY)
	require.True(t, ok)
	require.Equal(t, id, hitID)
	require.Equal(t, "copy this", text)

	closeX, closeY := closeButtonCoords(t, updated, id)
	updated, _ = updated.HandleMouseMotion(closeX, closeY)
	hitID, text, ok = updated.CopyHit(closeX, closeY)
	require.False(t, ok)
	require.Zero(t, hitID)
	require.Empty(t, text)
}

func TestNotification_HoverAndCopiedLabels(t *testing.T) {
	t.Parallel()

	n := New()
	n.SetSize(120, 50)
	updated, _ := n.Update(ShowMsg{Text: "copyable notification with enough width", Type: TypeInfo})
	id := updated.items[0].ID
	bounds := updated.itemBounds()[0]

	updated, _ = updated.HandleMouseMotion(bounds.col+1, bounds.row+1)
	require.Contains(t, ansi.Strip(updated.View()), hoverLabel)

	updated = updated.MarkCopied(id)
	require.Contains(t, ansi.Strip(updated.View()), copiedLabel)

	closeX, closeY := closeButtonCoords(t, updated, id)
	updated, _ = updated.HandleMouseMotion(closeX, closeY)
	require.Contains(t, ansi.Strip(updated.View()), closeLabel)

	updated, _ = updated.HandleMouseMotion(0, 20)
	require.NotContains(t, ansi.Strip(updated.View()), copiedLabel)
}

func TestNotification_HideAllClearsCopiedState(t *testing.T) {
	t.Parallel()

	n := New()
	n.SetSize(100, 50)
	updated, _ := n.Update(ShowMsg{Text: "copy", Type: TypeInfo})
	id := updated.items[0].ID
	updated = updated.MarkCopied(id)
	require.Equal(t, id, updated.copiedID)

	updated, _ = updated.Update(HideMsg{})
	require.Empty(t, updated.items)
	require.Zero(t, updated.hoveredID)
	require.Zero(t, updated.closeHoveredID)
	require.Zero(t, updated.copiedID)
}

func closeButtonCoords(t *testing.T, n Manager, id uint64) (int, int) {
	t.Helper()
	for _, b := range n.itemBounds() {
		if b.id != id {
			continue
		}
		return closeButtonPosition(b)
	}
	t.Fatalf("notification %d not found", id)
	return 0, 0
}

func stringsIndex(t *testing.T, s, substr string) int {
	t.Helper()
	idx := strings.Index(s, substr)
	require.NotEqual(t, -1, idx)
	return idx
}

var _ tea.Msg = AutoHideMsg{}

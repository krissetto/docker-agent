package sidebar

import (
	"strings"
	"testing"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/service"
)

// dollarOffset returns the index of "$" within a rendered (ANSI-stripped)
// usage reading line, i.e. the start of the cost segment.
func dollarOffset(t *testing.T, renderedLine string) int {
	t.Helper()
	idx := strings.Index(ansi.Strip(renderedLine), "$")
	require.GreaterOrEqual(t, idx, 0, "usage reading line must contain a cost figure")
	return idx
}

// TestSidebar_HandleClickType_Usage_Vertical verifies that the vertical Token
// Usage reading line splits between ClickUsageContext (glyph, tokens,
// context %) and ClickUsage (cost, sub-sessions), while the section's title
// line is no longer a click target.
func TestSidebar_HandleClickType_Usage_Vertical(t *testing.T) {
	t.Parallel()

	sess := session.New()
	sessionState := service.NewSessionState(sess)
	sb := New(animation.NewRuntime(), t.Context(), sessionState)

	m := sb.(*model)
	m.sessionHasContent = true
	m.titleGenerated = true
	m.sessionTitle = "Test"
	m.width = 40
	m.height = 50

	// Force a render to record the usage click zone
	_ = sb.View()

	require.GreaterOrEqual(t, m.usageReadingLine, 0, "the usage reading line must be recorded")
	require.Less(t, m.usageReadingLine, m.usageSectionEnd)

	assert.NotContains(t, ansi.Strip(strings.Join(m.cachedLines, "\n")), "Token Usage")
	paddingLeft := m.layoutCfg.PaddingLeft
	var result ClickResult

	result, _ = sb.HandleClickType(paddingLeft+2, m.usageReadingLine)
	assert.Equal(t, ClickUsageContext, result, "a click on the token/context segment should report ClickUsageContext")

	dollarX := dollarOffset(t, m.cachedLines[m.usageReadingLine])
	result, _ = sb.HandleClickType(paddingLeft+dollarX, m.usageReadingLine)
	assert.Equal(t, ClickUsage, result, "a click on the cost segment should report ClickUsage")
}

// TestSidebar_HandleClickType_Usage_Vertical_Compacting verifies that a click
// on the "(compacting\u2026)" marker reports ClickUsageContext: compaction is
// context-related, so it belongs on the context side, not the cost side.
func TestSidebar_HandleClickType_Usage_Vertical_Compacting(t *testing.T) {
	t.Parallel()

	sess := session.New()
	sessionState := service.NewSessionState(sess)
	sb := New(animation.NewRuntime(), t.Context(), sessionState)

	m := sb.(*model)
	m.sessionHasContent = true
	m.titleGenerated = true
	m.sessionTitle = "Test"
	m.width = 40
	m.height = 50
	m.compacting = true

	_ = sb.View()

	require.GreaterOrEqual(t, m.usageReadingLine, 0)
	assert.Contains(t, ansi.Strip(m.cachedLines[m.usageReadingLine]), "compacting")

	paddingLeft := m.layoutCfg.PaddingLeft
	result, _ := sb.HandleClickType(paddingLeft+2, m.usageReadingLine)
	assert.Equal(t, ClickUsageContext, result, "a click on the compacting marker should report ClickUsageContext")
}

// TestSidebar_HandleClickType_Usage_Vertical_Capped verifies that a click on
// the "⚠ capped" marker reports ClickUsageContext: a compaction-model cap
// on the effective context window is context-related, not cost-related.
func TestSidebar_HandleClickType_Usage_Vertical_Capped(t *testing.T) {
	t.Parallel()

	sess := session.New()
	sessionState := service.NewSessionState(sess)
	sb := New(animation.NewRuntime(), t.Context(), sessionState)

	m := sb.(*model)
	m.sessionHasContent = true
	m.titleGenerated = true
	m.sessionTitle = "Test"
	m.width = 40
	m.height = 50
	m.agentCompactionModel = "some/model"

	_ = sb.View()

	require.GreaterOrEqual(t, m.usageReadingLine, 0)
	line := ansi.Strip(m.cachedLines[m.usageReadingLine])
	require.Contains(t, line, "⚠ capped")

	paddingLeft := m.layoutCfg.PaddingLeft
	cappedX := strings.Index(line, "⚠")
	require.GreaterOrEqual(t, cappedX, 0)
	result, _ := sb.HandleClickType(paddingLeft+cappedX, m.usageReadingLine)
	assert.Equal(t, ClickUsageContext, result, "a click on the ⚠ capped marker should report ClickUsageContext")

	dollarX := dollarOffset(t, m.cachedLines[m.usageReadingLine])
	result, _ = sb.HandleClickType(paddingLeft+dollarX, m.usageReadingLine)
	assert.Equal(t, ClickUsage, result, "a click on the cost segment should still report ClickUsage when capped")
}

// TestSidebar_HandleClickType_Usage_Vertical_Hidden verifies a hidden usage
// section leaves no stale click zone behind.
func TestSidebar_HandleClickType_Usage_Vertical_Hidden(t *testing.T) {
	t.Parallel()

	sess := session.New()
	sessionState := service.NewSessionState(sess)
	sb := New(animation.NewRuntime(), t.Context(), sessionState)

	m := sb.(*model)
	m.width = 40
	m.height = 50
	m.SetSectionVisibility(SectionVisibility{HideUsage: true})

	_ = sb.View()

	paddingLeft := m.layoutCfg.PaddingLeft
	for y := range len(m.cachedLines) {
		result, _ := sb.HandleClickType(paddingLeft+2, y)
		assert.NotEqualf(t, ClickUsage, result, "hidden usage must not be clickable (line %d)", y)
	}
}

// TestSidebar_HandleClickType_Usage_Vertical_ScrollbarNotUsage verifies that
// when the vertical sidebar overflows, a click on the scrollbar column beside
// a Token Usage row is not swallowed as ClickUsage.
func TestSidebar_HandleClickType_Usage_Vertical_ScrollbarNotUsage(t *testing.T) {
	t.Parallel()

	sess := session.New()
	sessionState := service.NewSessionState(sess)
	sb := New(animation.NewRuntime(), t.Context(), sessionState)

	m := sb.(*model)
	m.sessionHasContent = true
	m.titleGenerated = true
	m.sessionTitle = "Test"
	m.width = 40
	m.height = 4 // force overflow with real queued content below usage
	m.SetQueuedMessages([]QueuedMessage{{ID: "one", Text: "queued one"}, {ID: "two", Text: "queued two"}, {ID: "three", Text: "queued three"}, {ID: "four", Text: "queued four"}})

	_ = sb.View()

	require.True(t, m.cachedNeedsScrollbar, "the sidebar must overflow for this test")
	require.GreaterOrEqual(t, m.usageReadingLine, 0)

	paddingLeft := m.layoutCfg.PaddingLeft
	scrollbarX := paddingLeft + m.contentWidth(true) // first non-content column
	for y := m.usageReadingLine; y < m.usageSectionEnd; y++ {
		result, _ := sb.HandleClickType(scrollbarX, y)
		assert.Equalf(t, ClickNone, result, "scrollbar column beside usage line %d must not be clickable content", y)
	}
}

// TestSidebar_HandleClickType_Usage_Collapsed_SharedLine verifies the
// right-aligned usage reading on the shared path/usage row splits between
// ClickUsageContext and ClickUsage the same way the vertical reading does,
// while the path part keeps reporting ClickWorkingDir.
func TestSidebar_HandleClickType_Usage_Collapsed_SharedLine(t *testing.T) {
	t.Parallel()

	sess := session.New()
	sessionState := service.NewSessionState(sess)
	sb := New(animation.NewRuntime(), t.Context(), sessionState)

	m := sb.(*model)
	m.sessionHasContent = true
	m.titleGenerated = true
	m.mode = ModeCollapsed
	m.width = 80
	m.sessionTitle = "Hi"
	m.workingDirectory = "~/projects/myapp"

	vm := m.computeCollapsedViewModel(m.contentWidth(false))
	require.True(t, vm.WdAndUsageOnOneLine, "path and usage must share one line at this width")
	require.NotEmpty(t, vm.UsageSummary)

	paddingLeft := m.layoutCfg.PaddingLeft
	rowY := vm.titleSectionLines()
	usageX := vm.ContentWidth - lipgloss.Width(vm.UsageSummary)

	result, _ := sb.HandleClickType(paddingLeft+usageX+1, rowY)
	assert.Equal(t, ClickUsageContext, result, "click on the token/context segment should report ClickUsageContext")

	dollarX := dollarOffset(t, vm.UsageSummary)
	result, _ = sb.HandleClickType(paddingLeft+usageX+dollarX, rowY)
	assert.Equal(t, ClickUsage, result, "click on the cost segment should report ClickUsage")

	result, _ = sb.HandleClickType(paddingLeft+3, rowY)
	assert.Equal(t, ClickWorkingDir, result, "the path part of the row stays a working dir target")
}

// TestSidebar_HandleClickType_Usage_Collapsed_OwnLine verifies the usage
// reading splits between ClickUsageContext and ClickUsage when it takes its
// own row (no session path).
func TestSidebar_HandleClickType_Usage_Collapsed_OwnLine(t *testing.T) {
	t.Parallel()

	sess := session.New()
	sessionState := service.NewSessionState(sess)
	sb := New(animation.NewRuntime(), t.Context(), sessionState)

	m := sb.(*model)
	m.sessionHasContent = true
	m.titleGenerated = true
	m.mode = ModeCollapsed
	m.width = 50
	m.sessionTitle = "Hi"
	m.workingDirectory = ""

	vm := m.computeCollapsedViewModel(m.contentWidth(false))
	require.NotEmpty(t, vm.UsageSummary)

	paddingLeft := m.layoutCfg.PaddingLeft
	rowY := vm.titleSectionLines()
	usageX := vm.ContentWidth - lipgloss.Width(vm.UsageSummary)

	result, _ := sb.HandleClickType(paddingLeft+usageX+1, rowY)
	assert.Equal(t, ClickUsageContext, result, "click on the token/context segment should report ClickUsageContext")

	dollarX := dollarOffset(t, vm.UsageSummary)
	result, _ = sb.HandleClickType(paddingLeft+usageX+dollarX, rowY)
	assert.Equal(t, ClickUsage, result, "click on the cost segment should report ClickUsage")
}

// TestSidebar_HandleClickType_Usage_Collapsed_Capped verifies the collapsed
// band's own-line usage reading also puts the "⚠ capped" marker on the
// context side, inheriting the fix through the shared usageContextSegWidth.
func TestSidebar_HandleClickType_Usage_Collapsed_Capped(t *testing.T) {
	t.Parallel()

	sess := session.New()
	sessionState := service.NewSessionState(sess)
	sb := New(animation.NewRuntime(), t.Context(), sessionState)

	m := sb.(*model)
	m.sessionHasContent = true
	m.titleGenerated = true
	m.mode = ModeCollapsed
	m.width = 50
	m.sessionTitle = "Hi"
	m.workingDirectory = ""
	m.agentCompactionModel = "some/model"

	vm := m.computeCollapsedViewModel(m.contentWidth(false))
	require.NotEmpty(t, vm.UsageSummary)
	require.Contains(t, vm.UsageSummary, "⚠ capped")

	paddingLeft := m.layoutCfg.PaddingLeft
	rowY := vm.titleSectionLines()
	usageX := vm.ContentWidth - lipgloss.Width(vm.UsageSummary)

	cappedX := strings.Index(ansi.Strip(vm.UsageSummary), "⚠")
	require.GreaterOrEqual(t, cappedX, 0)
	result, _ := sb.HandleClickType(paddingLeft+usageX+cappedX, rowY)
	assert.Equal(t, ClickUsageContext, result, "click on the ⚠ capped marker should report ClickUsageContext")

	dollarX := dollarOffset(t, vm.UsageSummary)
	result, _ = sb.HandleClickType(paddingLeft+usageX+dollarX, rowY)
	assert.Equal(t, ClickUsage, result, "click on the cost segment should still report ClickUsage when capped")
}

// TestSidebar_HandleClickType_Usage_Collapsed_Hidden verifies a hidden usage
// section keeps no hit target in the collapsed band.
func TestSidebar_HandleClickType_Usage_Collapsed_Hidden(t *testing.T) {
	t.Parallel()

	sess := session.New()
	sessionState := service.NewSessionState(sess)
	sb := New(animation.NewRuntime(), t.Context(), sessionState)

	m := sb.(*model)
	m.mode = ModeCollapsed
	m.width = 50
	m.sessionTitle = "Hi"
	m.SetSectionVisibility(SectionVisibility{HideUsage: true})

	paddingLeft := m.layoutCfg.PaddingLeft
	for y := range 6 {
		result, _ := sb.HandleClickType(paddingLeft+3, y)
		assert.NotEqualf(t, ClickUsage, result, "hidden usage must not be clickable (line %d)", y)
	}
}

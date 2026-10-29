package messages

import (
	"reflect"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tui/styles"
	"github.com/docker/docker-agent/pkg/tui/types"
)

// expandedWorkHandoffFixture holds an active reply handoff whose old layout includes an
// expanded Work block with tool rows, so handoff rows carry Composite owners and sources.
func expandedWorkHandoffFixture(t *testing.T) *model {
	t.Helper()
	m := newFourthReplyFixture(t)
	for _, id := range []string{"fourth-reply-memo-call", "fourth-reply-memo-call-2"} {
		call := tools.ToolCall{ID: id, Function: tools.FunctionCall{Name: "check"}}
		m.AddOrUpdateToolCall("root", call, tools.Tool{Name: "check"}, types.ToolStatusCompleted)
	}
	m.addMessage(types.Agent(types.MessageTypeAssistant, "root", ""))
	m.View()
	for _, block := range m.workBlocks {
		block.SetExpandedImmediately(true)
	}
	m.invalidateAllItems()
	m.View()
	// Long wrapping prose so a width change re-wraps the visible rows.
	m.AppendToLastMessage("root", strings.Repeat("wrapped reply prose words ", 12))
	m.View()
	return m
}

// directHandoffRowTexts recomputes every sourced row through the unmemoised path.
func directHandoffRowTexts(m *model) []string {
	rows := m.replyHandoff.Rows()
	texts := make([]string, len(rows))
	for i := range rows {
		if rows[i].Source != nil {
			texts[i] = m.sourceTextRow(rows[i])
		}
	}
	return texts
}

// memoHandoffRowTexts runs the memoised sync pass and reads back the sourced rows.
func memoHandoffRowTexts(m *model) []string {
	m.syncAnimatedWorkProjection()
	texts := make([]string, len(m.replyHandoffFrameRows))
	for i, row := range m.replyHandoffFrameRows {
		if row.Source != nil {
			texts[i] = row.Text
		}
	}
	return texts
}

// requireMemoMatchesDirect asserts byte-identical sourced row text (memo vs direct) and
// returns the stage's texts plus sourced and composite row counts.
func requireMemoMatchesDirect(t *testing.T, m *model, label string) (texts []string, sourced, composite int) {
	t.Helper()
	require.True(t, m.replyHandoff.Active(), "%s: handoff must be active", label)
	direct := directHandoffRowTexts(m)
	memo := memoHandoffRowTexts(m)
	require.Len(t, memo, len(direct), "%s: row count", label)
	for i := range direct {
		require.Equal(t, direct[i], memo[i], "%s: row %d text must be byte-identical", label, i)
	}
	for _, row := range m.replyHandoff.Rows() {
		if row.Source != nil {
			sourced++
		}
		if row.Source != nil && row.Composite != nil {
			composite++
		}
	}
	require.Positive(t, sourced, "%s: sourced rows", label)
	require.Positive(t, composite, "%s: composite rows", label)
	return memo, sourced, composite
}

// recolourTheme clones the current theme and rewrites every hex colour, so the
// recomputed rows must differ from the pre-change rows.
func recolourTheme(t *testing.T, base *styles.Theme) *styles.Theme {
	t.Helper()
	changed := *base
	colors := reflect.ValueOf(&changed.Colors).Elem()
	for _, field := range colors.Fields() {
		if field.Kind() == reflect.String && strings.HasPrefix(field.String(), "#") {
			field.SetString("#c0ffee")
		}
	}
	return &changed
}

// The per-pass memo must return byte-identical row text, including composite rows,
// and must stay identical while width and theme change mid-handoff.
func TestHandoffSyncMemoMatchesDirectRowTextAcrossWidthAndTheme(t *testing.T) {
	m := expandedWorkHandoffFixture(t)
	initial, sourced, composite := requireMemoMatchesDirect(t, m, "initial")
	t.Logf("initial stage: sourced=%d composite=%d", sourced, composite)

	m.SetSize(100, 40)
	m.View()
	widthTexts, sourced, composite := requireMemoMatchesDirect(t, m, "after width change")
	t.Logf("width stage: sourced=%d composite=%d", sourced, composite)
	require.NotEqual(t, initial, widthTexts, "the width change must recompute visible row text")

	previous := styles.CurrentTheme()
	t.Cleanup(func() { styles.ApplyTheme(previous) })
	generation := styles.ThemeGeneration()
	styles.ApplyTheme(recolourTheme(t, previous))
	require.NotEqual(t, generation, styles.ThemeGeneration(), "applying a theme bumps the theme generation")
	m.View()
	themeTexts, sourced, composite := requireMemoMatchesDirect(t, m, "after theme change")
	t.Logf("theme stage: sourced=%d composite=%d", sourced, composite)
	require.NotEqual(t, widthTexts, themeTexts, "the theme change must recompute visible row text")
}

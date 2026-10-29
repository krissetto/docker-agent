package tui

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	agentruntime "github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
)

const tableOverflowFixture = "| what | directory | branch / HEAD |\n" +
	"| --- | --- | --- |\n" +
	"| Cloud adoption (open this PR) | `/Users/krissetto/dev/ai-dev/.tmp-public-api-unified-sdk/cloud-api-simplified` | `cloud-api-simplified` @ `e3c7c1b` on `b0efa7d` |\n" +
	"| Spec trim (parked) | `/Users/krissetto/dev/ai-dev/.tmp-public-api-unified-sdk/worktrees-generation-comparison/spec-trim` | `spec-trim` @ `8f3d8d5` on `b0efa7d` |\n\n"

func TestTableRootKeepsWrappedCellsInColumns(t *testing.T) {
	for _, width := range []int{60, 100, 160} {
		t.Run(strconv.Itoa(width), func(t *testing.T) {
			root, _, _ := frozenClockRoot(t, width, 120)
			if compact, ok := root.chatPage.(interface{ SetCompactWorkLog(enabled bool) tea.Cmd }); ok {
				compact.SetCompactWorkLog(false)
			}
			root.Update(agentruntime.StreamStarted("profile", "root"))
			for chunk := range strings.SplitSeq(tableOverflowFixture, "\n") {
				root.Update(agentruntime.AgentChoice("root", "profile", chunk+"\n"))
				root.View()
			}
			live := root.View().Content
			root.Update(agentruntime.StreamStopped("profile", "root", "completed"))
			final := root.View().Content

			replay, _, _ := frozenClockRoot(t, width, 120)
			if compact, ok := replay.chatPage.(interface{ SetCompactWorkLog(enabled bool) tea.Cmd }); ok {
				compact.SetCompactWorkLog(false)
			}
			msg := session.NewAgentMessage("root", &chat.Message{Role: chat.MessageRoleAssistant, Content: tableOverflowFixture})
			replay.application.Session().Messages = append(replay.application.Session().Messages, session.NewMessageItem(msg))
			replay.chatPage.Init()
			replay.viewCacheValid = false
			replayed := replay.View().Content

			var expected []string
			for _, state := range []struct{ name, frame string }{{"live", live}, {"final", final}, {"replay", replayed}} {
				if dir := os.Getenv("DOCKER_AGENT_TUI_FIXTURE_DIR"); dir != "" {
					require.NoError(t, os.WriteFile(filepath.Join(dir, fmt.Sprintf("table-%d-%s.ansi.txt", width, state.name)), []byte(state.frame), 0o600))
				}
				rows := rootTableRows(t, state.frame)
				if expected == nil {
					expected = rows
				} else {
					require.Equal(t, expected, rows, "%s uses the same column layout", state.name)
				}
			}
		})
	}
}

func rootTableRows(t *testing.T, frame string) []string {
	t.Helper()
	var rows []string
	var expectedColumns []int
	started := false
	for styled := range strings.SplitSeq(frame, "\n") {
		row := ansi.Strip(styled)
		if !started {
			if strings.Count(row, "│") != 2 {
				continue
			}
			started = true
		}
		if strings.TrimSpace(row) == "" {
			break
		}
		var columns []int
		for index, char := range row {
			if char == '│' || char == '┼' {
				columns = append(columns, ansi.StringWidth(row[:index]))
			}
		}
		require.Len(t, columns, 2, "every table continuation row retains both column boundaries: %q", row)
		if expectedColumns == nil {
			expectedColumns = columns
		} else {
			require.Equal(t, expectedColumns, columns, "table columns do not drift: %q", row)
		}
		rows = append(rows, strings.TrimSpace(row))
	}
	require.Greater(t, len(rows), 3, "the rendered table contains wrapped data rows")
	return rows
}

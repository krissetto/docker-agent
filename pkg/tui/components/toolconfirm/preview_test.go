package toolconfirm

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/subagent"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tools/builtin/handoff"
	"github.com/docker/docker-agent/pkg/tools/builtin/transfertask"
	"github.com/docker/docker-agent/pkg/tui/components/markdown"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

func TestPreviewProposedDelegation(t *testing.T) {
	for _, tt := range []struct {
		name string
		args any
		want []string
	}{
		{subagent.ToolSpawnSubagent, subagent.SpawnArgs{Agent: "reviewer", Task: "Review **approval** handling.\n\nReport regressions only."}, []string{"agent:", "reviewer", "task:", "approval", "Report regressions only."}},
		{transfertask.ToolNameTransferTask, transfertask.Args{Agent: "reviewer", Task: "Inspect permissions", ExpectedOutput: "A concise report"}, []string{"reviewer", "Inspect permissions", "expected output:", "A concise report"}},
		{handoff.ToolNameHandoff, handoff.Args{Agent: "reviewer"}, []string{"agent:", "reviewer"}},
		{subagent.ToolSendMessage, subagent.SendArgs{To: "0123456789abcdef0123456789abcdef", Message: "Please inspect this finding"}, []string{"to:", "0123456789abcdef0123456789abcdef", "Please inspect this finding"}},
		{subagent.ToolSendMessage, subagent.SendArgs{To: subagent.ParentAlias, Message: "Decision needed"}, []string{"parent", "Decision needed"}},
		{subagent.ToolReadSubagent, subagent.ReadArgs{SubagentID: "child-id", LastMessages: 5, Full: true}, []string{"child-id", "last messages:", "5", "full:", "true"}},
		{subagent.ToolStopSubagent, subagent.StopArgs{SubagentID: "child-id"}, []string{"subagent id:", "child-id"}},
	} {
		t.Run(tt.name+fmt.Sprint(tt.args), func(t *testing.T) {
			args, err := json.Marshal(tt.args)
			require.NoError(t, err)
			call := tools.ToolCall{ID: "unexecuted-call", Function: tools.FunctionCall{Name: tt.name, Arguments: string(args)}}
			definition := tools.Tool{Name: tt.name, Handler: func(context.Context, tools.ToolCall, tools.Runtime) (*tools.ToolCallResult, error) {
				t.Fatal("preview executed the proposed tool")
				return nil, nil
			}}
			view := Preview(call, definition, 80)
			plain := ansi.Strip(view)
			assert.Contains(t, plain, "Proposed: "+tt.name)
			for _, want := range tt.want {
				assert.Contains(t, plain, want)
			}
			for _, live := range []string{"Spawning", "Spawned", "Inspecting", "Stopping", "Messaging", "Start", "unexecuted-call", "⠋", "✓"} {
				assert.NotContains(t, plain, live)
			}
			assert.Equal(t, view, Preview(call, definition, 80))
			if tt.name == handoff.ToolNameHandoff {
				assert.NotContains(t, plain, "task:")
			}
		})
	}
}

func TestPreviewPreservesOtherToolInputs(t *testing.T) {
	for _, tt := range []struct {
		name string
		args string
		want []string
	}{
		{"shell", `{"cmd":"printf '%s' hello\nprintf goodbye","cwd":"/tmp"}`, []string{"printf '%s' hello", "printf goodbye", "/tmp"}},
		{"run_background_job", `{"cmd":" ","command":"npm run dev","cwd":"/workspace","recall":true}`, []string{"cmd:", "command:", "npm run dev", "/workspace", "recall:", "true"}},
		{"write_file", `{"path":"/tmp/input.md","content":"# Literal heading\n**literal syntax**"}`, []string{"/tmp/input.md", "# Literal heading", "**literal syntax**"}},
		{"search", `{"query":"Find **approval** checks","options":{"limit":9007199254740993},"paths":["/a","/b"],"optional":null}`, []string{"approval", "9007199254740993", `"/a"`, `"/b"`, "null"}},
		{"custom", `{"task":"partial`, []string{`{"task":"partial`}},
		{"custom", `["one",2]`, []string{`["one",2]`}},
		{"custom", `null`, []string{"null"}},
		{"custom", ``, nil},
		{"custom", `{}`, nil},
	} {
		t.Run(tt.name+tt.args, func(t *testing.T) {
			call := tools.ToolCall{Function: tools.FunctionCall{Name: tt.name, Arguments: tt.args}}
			plain := ansi.Strip(Preview(call, tools.Tool{}, 80))
			assert.Contains(t, plain, "Proposed: "+tt.name)
			for _, want := range tt.want {
				assert.Contains(t, plain, want)
			}
		})
	}
}

func TestPreviewBoundedWidthAndMultiline(t *testing.T) {
	args, err := json.Marshal(map[string]any{
		"agent": "reviewer-with-a-long-name",
		"task":  "# Approval\n\n- Inspect 界面 and **permissions**.\n\n```text\n" + strings.Repeat("long-token", 12) + "\n```\n\nLAST-PARAGRAPH",
		"path":  "/a/very/long/unbroken/path/to/input.md",
	})
	require.NoError(t, err)
	call := tools.ToolCall{Function: tools.FunctionCall{Name: subagent.ToolSpawnSubagent, Arguments: string(args)}}
	for _, width := range []int{8, 16, 32, 80} {
		t.Run(strconv.Itoa(width), func(t *testing.T) {
			view := Preview(call, tools.Tool{}, width)
			for line := range strings.SplitSeq(view, "\n") {
				assert.LessOrEqual(t, ansi.StringWidth(line), width, "%q", line)
			}
			assert.Contains(t, strings.Join(strings.Fields(ansi.Strip(view)), ""), "LAST-PARAGRAPH")
			assert.NotContains(t, view, markdown.CodeBlockCopyIcon)
		})
	}
	for _, width := range []int{-1, 0, 1} {
		view := Preview(tools.ToolCall{Function: tools.FunctionCall{Name: "x", Arguments: `{"task":"text"}`}}, tools.Tool{}, width)
		for line := range strings.SplitSeq(view, "\n") {
			assert.LessOrEqual(t, ansi.StringWidth(line), 1)
		}
	}
}

func TestPreviewUsesCurrentThemeAndDisplayName(t *testing.T) {
	original := styles.CurrentTheme()
	t.Cleanup(func() { styles.ApplyTheme(original) })
	call := tools.ToolCall{Function: tools.FunctionCall{Name: subagent.ToolSpawnSubagent, Arguments: `{"agent":"reviewer","task":"# Approval\n\n**Check** the permissions."}`}}
	definition := tools.Tool{Annotations: tools.ToolAnnotations{Title: "Spawn Subagent"}}
	before := Preview(call, definition, 60)
	assert.Contains(t, ansi.Strip(before), "Proposed: Spawn Subagent")
	theme := *original
	theme.Colors.Background, theme.Colors.TextPrimary = "#ffffff", "#111111"
	theme.Markdown.Heading = "#112233"
	styles.ApplyTheme(&theme)
	after := Preview(call, definition, 60)
	assert.NotEqual(t, before, after)
	assert.Equal(t, after, Preview(call, definition, 60))
	assert.Equal(t, ansi.Strip(before), ansi.Strip(after))
}

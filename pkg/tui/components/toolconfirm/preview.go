package toolconfirm

import (
	"encoding/json"
	"maps"
	"slices"
	"strings"

	"github.com/charmbracelet/x/ansi"

	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tui/components/markdown"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

// Preview renders only the proposed call's inputs. It never constructs a live
// tool component; the containing dialog owns scrolling and approval actions.
func Preview(toolCall tools.ToolCall, toolDefinition tools.Tool, width int) string {
	width = max(1, width)
	name := toolDefinition.DisplayName()
	if name == "" {
		name = toolCall.Function.Name
	}
	parts := []string{styles.SecondaryStyle.Render(ansi.Wrap("Proposed: "+name, width, ""))}
	arguments := toolCall.Function.Arguments
	if strings.TrimSpace(arguments) == "" {
		return parts[0]
	}

	var args map[string]json.RawMessage
	if err := json.Unmarshal([]byte(arguments), &args); err != nil || args == nil {
		return parts[0] + "\n\n" + previewLiteral(arguments, width)
	}

	// Put the recipient and assignment first, without dropping other inputs.
	keys := make([]string, 0, len(args))
	for _, key := range []string{"agent", "to", "subagent_id", "task", "query", "message", "expected_output"} {
		if _, ok := args[key]; ok {
			keys = append(keys, key)
		}
	}
	for _, key := range slices.Sorted(maps.Keys(args)) {
		if !slices.Contains(keys, key) {
			keys = append(keys, key)
		}
	}
	for _, key := range keys {
		label := strings.ReplaceAll(key, "_", " ") + ":"
		value := args[key]
		var text string
		var content string
		if err := json.Unmarshal(value, &text); err == nil && string(value) != "null" {
			switch key {
			case "task", "query", "message", "expected_output":
				content, err = markdown.NewRendererWithoutCopyIcon(width).Render(text)
				if err != nil {
					content = previewLiteral(text, width)
				}
			default:
				content = previewLiteral(text, width)
			}
		} else {
			// RawMessage retains large numbers and nested values exactly.
			formatted, err := json.MarshalIndent(value, "", "  ")
			if err != nil {
				formatted = value
			}
			content = previewLiteral(string(formatted), width)
		}
		parts = append(parts, styles.MutedStyle.Render(ansi.Wrap(label, width, ""))+"\n"+content)
	}
	// Markdown decorations and long literal tokens must fit the same body lane.
	return ansi.Wrap(strings.Join(parts, "\n\n"), width, "")
}

func previewLiteral(text string, width int) string {
	return styles.DialogContentStyle.Render(ansi.Wrap(strings.ReplaceAll(text, "\t", "    "), width, ""))
}

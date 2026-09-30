package message

import (
	"strings"

	"charm.land/lipgloss/v2"
)

type assistantTailLines struct {
	input    []string
	rendered [][]string
}

// Markdown normally closes styles and links on every line. Other terminal
// control sequences require the original whole-string renderer's state.
func independentStyledLine(line string) bool {
	styleOpen, linkOpen := false, false
	for i := 0; i < len(line); i++ {
		if line[i] != '\x1b' {
			continue
		}
		switch {
		case strings.HasPrefix(line[i:], "\x1b["):
			end := i + 2
			for end < len(line) && line[end] >= '0' && line[end] <= '?' {
				end++
			}
			if end >= len(line) || line[end] != 'm' {
				return false
			}
			styleOpen = end != i+2 && line[i+2:end] != "0"
			i = end
		case strings.HasPrefix(line[i:], "\x1b]8;"):
			start := i + 4
			semi := strings.IndexByte(line[start:], ';')
			if semi < 0 {
				return false
			}
			uri := start + semi + 1
			end := uri
			for end < len(line) && line[end] != '\a' && line[end] != '\x1b' {
				end++
			}
			if end == len(line) {
				return false
			}
			linkOpen = end > uri
			if line[end] == '\x1b' {
				if end+1 >= len(line) || line[end+1] != '\\' {
					return false
				}
				end++
			}
			i = end
		default:
			return false
		}
	}
	return !styleOpen && !linkOpen
}

func (c *assistantTailLines) render(style lipgloss.Style, width int, content string) []string {
	if content == "" {
		*c = assistantTailLines{}
		return nil
	}
	lines := strings.Split(content, "\n")
	for _, line := range lines {
		if !independentStyledLine(line) {
			*c = assistantTailLines{}
			return styledAssistantLines(style, width, content)
		}
	}
	shared := 0
	for shared < len(lines) && shared < len(c.input) && lines[shared] == c.input[shared] {
		shared++
	}
	clear(c.rendered[shared:])
	c.rendered = c.rendered[:shared]
	for _, line := range lines[shared:] {
		if line == "" {
			c.rendered = append(c.rendered, strings.Split(style.Width(width).Render(""), "\n"))
		} else {
			c.rendered = append(c.rendered, styledAssistantLines(style, width, line))
		}
	}
	c.input = lines
	var result []string
	for _, rendered := range c.rendered {
		result = append(result, rendered...)
	}
	return result
}

package message

import (
	"fmt"
	"testing"

	"github.com/docker/docker-agent/pkg/tui/animation"
	"github.com/docker/docker-agent/pkg/tui/types"
)

func withinChunks(kind string, n int) []string {
	chunks := make([]string, 0, n+2)
	if kind == "code" {
		chunks = append(chunks, "```text\n")
	}
	if kind == "go" {
		chunks = append(chunks, "```go\n")
	}
	if kind == "table" {
		chunks = append(chunks, "| key | value |\n| --- | --- |\n")
	}
	for i := range n {
		switch kind {
		case "code":
			chunks = append(chunks, fmt.Sprintf("line %04d deterministic plain code with local content\n", i))
		case "go":
			chunks = append(chunks, fmt.Sprintf("var value%04d = %d // deterministic code\n", i, i))
		case "list":
			chunks = append(chunks, fmt.Sprintf("- item %04d **bold** text and `code` details\n", i))
		case "table":
			chunks = append(chunks, fmt.Sprintf("| item%04d | deterministic cell %d |\n", i, i))
		case "paragraph":
			chunks = append(chunks, fmt.Sprintf("word%04d **bold** and `code` deterministic prose ", i))
		}
	}
	if kind == "code" || kind == "go" {
		chunks = append(chunks, "```\n")
	}
	return chunks
}
func BenchmarkStreamingMutableBlocks(b *testing.B) {
	for _, kind := range []string{"code", "go", "list", "table", "paragraph"} {
		b.Run(kind, func(b *testing.B) {
			chunks := withinChunks(kind, 150)
			var size int
			for _, chunk := range chunks {
				size += len(chunk)
			}
			b.SetBytes(int64(size))
			b.ReportAllocs()
			for b.Loop() {
				view := New(animation.NewRuntime(), types.Agent(types.MessageTypeAssistant, "root", ""), nil)
				for _, chunk := range chunks {
					view.AppendContent(chunk)
					view.RenderedSegments(100)
				}
				view.Finalize()
			}
		})
	}
}

package builtins_test

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"

	"github.com/docker/docker-agent/pkg/hooks/builtins"
)

func TestBoundToolResultAllowList(t *testing.T) {
	t.Parallel()
	for _, category := range []string{"filesystem", "shell", "mcp", "a2a", "background_jobs", "memory", "fetch", "skills", "", "custom"} {
		t.Run(category, func(t *testing.T) {
			t.Parallel()
			payload := strings.Repeat("世", 50*1024)
			got := builtins.BoundToolResult(category, "test", payload, "Output could not be saved.")
			switch category {
			case "filesystem", "shell", "mcp", "a2a", "background_jobs":
				assert.LessOrEqual(t, len(got), 50*1024)
				assert.True(t, utf8.ValidString(got))
				assert.Contains(t, got, "Output could not be saved")
				assert.Equal(t, got, builtins.BoundToolResult(category, "test", got, "Output could not be saved."))
			default:
				assert.Equal(t, payload, got)
			}
		})
	}
}

func TestBoundToolResultBoundaries(t *testing.T) {
	t.Parallel()
	for _, size := range []int{0, 50*1024 - 1, 50 * 1024, 50*1024 + 1} {
		payload := strings.Repeat("x", size)
		got := builtins.BoundToolResult("shell", "shell", payload, "Not saved.")
		assert.LessOrEqual(t, len(got), 50*1024)
		if size <= 50*1024 {
			assert.Equal(t, payload, got)
		} else {
			assert.Contains(t, got, "Not saved.")
		}
	}
}

func TestBoundToolResultBoundsNoticeAndPreservesReadFileHead(t *testing.T) {
	t.Parallel()
	payload := "important beginning\n" + strings.Repeat("世", 100_000) + "diagnostic tail"
	for _, category := range []string{"filesystem", "mcp"} {
		got := builtins.BoundToolResult(category, "read_file", payload, strings.Repeat("界", 100_000))
		assert.LessOrEqual(t, len(got), 50*1024)
		assert.True(t, utf8.ValidString(got))
		if category == "filesystem" {
			assert.Contains(t, got, "important beginning")
			assert.NotContains(t, got, "diagnostic tail")
		} else {
			assert.Contains(t, got, "diagnostic tail")
			assert.NotContains(t, got, "important beginning")
		}
	}
}

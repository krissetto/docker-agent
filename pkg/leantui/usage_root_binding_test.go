package leantui

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/docker/docker-agent/pkg/leantui/ui"
	"github.com/docker/docker-agent/pkg/runtime"
)

func TestBoundRootIgnoresChildUsageArrivingFirst(t *testing.T) {
	m := bareModel(80)
	m.usage = ui.NewUsageTracker()
	m.usage.BindRoot("root")

	m.setTokenUsage("child", &runtime.Usage{ContextLength: 90, ContextLimit: 100, InputTokens: 90})
	assert.Zero(t, m.status.ContextLength, "child-only usage must not drive the root footer")

	m.setTokenUsage("root", &runtime.Usage{ContextLength: 10, ContextLimit: 100, InputTokens: 10})
	assert.Equal(t, int64(10), m.status.ContextLength)
	assert.Equal(t, int64(100), m.status.ContextLimit)

	m.setTokenUsage("child", &runtime.Usage{ContextLength: 95, ContextLimit: 100, InputTokens: 95})
	assert.Equal(t, int64(10), m.status.ContextLength, "later child usage must not rebind the footer")
}

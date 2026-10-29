package root

import (
	"io"
	"testing"

	"github.com/stretchr/testify/assert"

	tuiimage "github.com/docker/docker-agent/pkg/tui/image"
)

func TestConfigureImageWriterSkipsProbeWhenDisabled(t *testing.T) {
	w := tuiimage.NewWriter(io.Discard)
	probed := false
	configureImageWriter(w, false, func() bool {
		probed = true
		return true
	})
	assert.False(t, probed, "disabled images must not probe the terminal")
	assert.False(t, w.Enabled())
	assert.False(t, w.Supported())
	assert.False(t, w.RenderingEnabled())
}

func TestConfigureImageWriterEnablesBeforeProbe(t *testing.T) {
	for _, supported := range []bool{false, true} {
		w := tuiimage.NewWriter(io.Discard)
		probed := false
		configureImageWriter(w, true, func() bool {
			probed = true
			assert.True(t, w.Enabled(), "the preference must be applied before the probe runs")
			return supported
		})
		assert.True(t, probed)
		assert.True(t, w.Enabled())
		assert.Equal(t, supported, w.Supported())
		assert.Equal(t, supported, w.RenderingEnabled())
	}
}

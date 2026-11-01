package userconfig

import (
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestInactivePaneDimDefaultsClampAndRejectNonFinite(t *testing.T) {
	t.Parallel()

	var nilSettings *Settings
	assert.InDelta(t, 0.5, nilSettings.GetInactivePaneDim(), 0)
	assert.InDelta(t, 0.5, (&Settings{}).GetInactivePaneDim(), 0)

	for input, want := range map[float64]float64{
		0:               0,
		0.37:            0.37,
		1:               1,
		-0.25:           0,
		1.5:             1,
		math.Inf(1):     0.5,
		math.Inf(-1):    0.5,
		math.NaN():      0.5,
		math.MaxFloat64: 1,
	} {
		value := input
		assert.InDelta(t, want, (&Settings{InactivePaneDim: &value}).GetInactivePaneDim(), 0, "input %v", input)
	}
}

func TestInactivePaneDimLoadsFromConfigFile(t *testing.T) {
	t.Parallel()

	cases := map[string]float64{
		"settings:\n  inactive_pane_dim: 0.25\n":  0.25,
		"settings:\n  inactive_pane_dim: 0\n":     0,
		"settings:\n  inactive_pane_dim: 0.0\n":   0,
		"settings:\n  inactive_pane_dim: 3\n":     1,
		"settings:\n  inactive_pane_dim: -1\n":    0,
		"settings:\n  inactive_pane_dim: .nan\n":  0.5,
		"settings:\n  inactive_pane_dim: .inf\n":  0.5,
		"settings:\n  inactive_pane_dim: -.inf\n": 0.5,
		"settings: {}\n":                          0.5,
	}
	for content, want := range cases {
		configFile := filepath.Join(t.TempDir(), "config.yaml")
		require.NoError(t, os.WriteFile(configFile, []byte(content), 0o644))
		config, err := loadFrom(configFile, "")
		require.NoError(t, err, content)
		assert.InDelta(t, want, config.GetSettings().GetInactivePaneDim(), 1e-12, content)
	}
}

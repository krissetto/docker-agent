package userconfig

import (
	"testing"

	"github.com/goccy/go-yaml"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPanelSettingsRoundTrip(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		panel *PanelSettings
	}{
		{"defaults", nil},
		{"off", &PanelSettings{Elements: []string{}}},
		{"ordered", &PanelSettings{Elements: []string{"todos", "workspace"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := Settings{Panel: tc.panel}
			data, err := yaml.Marshal(s)
			require.NoError(t, err)
			var loaded Settings
			require.NoError(t, yaml.Unmarshal(data, &loaded))
			assert.Equal(t, tc.panel, loaded.GetPanel())
			if tc.panel == nil {
				assert.NotContains(t, string(data), "panel:")
			} else if len(tc.panel.Elements) == 0 {
				assert.Contains(t, string(data), "elements: []")
			}
		})
	}
	var absent *Settings
	assert.Nil(t, absent.GetPanel())
	s := Settings{Panel: &PanelSettings{Elements: []string{"todos"}}}
	detached := s.GetPanel()
	detached.Elements[0] = "workspace"
	assert.Equal(t, "todos", s.Panel.Elements[0])
}

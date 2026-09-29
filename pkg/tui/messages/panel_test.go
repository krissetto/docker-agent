package messages

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNormalizePanelSettings(t *testing.T) {
	t.Parallel()
	assert.Equal(t, []PanelElement{PanelWorkspace, PanelSubagents, PanelTodos}, NormalizePanelSettings(PanelSettings{}).Elements)
	off := NormalizePanelSettings(PanelSettings{Elements: []PanelElement{}})
	require.NotNil(t, off.Elements)
	assert.Empty(t, off.Elements)
	input := PanelSettings{Elements: []PanelElement{PanelTodos, "unknown", PanelWorkspace, PanelTodos, "git"}}
	got := NormalizePanelSettings(input)
	assert.Equal(t, []PanelElement{PanelTodos, PanelWorkspace}, got.Elements)
	got.Elements[0] = PanelSubagents
	assert.Equal(t, PanelTodos, input.Elements[0], "normalization detaches caller data")
	unknown := NormalizePanelSettings(PanelSettings{Elements: []PanelElement{"unknown"}})
	require.NotNil(t, unknown.Elements)
	assert.Empty(t, unknown.Elements, "unknown-only explicit configuration stays off")
	defaults := DefaultPanelSettings()
	defaults.Elements[0] = PanelTodos
	assert.Equal(t, PanelWorkspace, DefaultPanelSettings().Elements[0])
}

func TestPreferencesPanelEquality(t *testing.T) {
	t.Parallel()
	assert.True(t, Preferences{}.Equal(Preferences{Panel: DefaultPanelSettings()}))
	assert.False(t, Preferences{}.Equal(Preferences{Panel: PanelSettings{Elements: []PanelElement{}}}))
	assert.False(t, Preferences{}.Equal(Preferences{Sound: true}))
	assert.False(t, (PanelSettings{Elements: []PanelElement{PanelWorkspace, PanelTodos}}).Equal(PanelSettings{Elements: []PanelElement{PanelTodos, PanelWorkspace}}))
}

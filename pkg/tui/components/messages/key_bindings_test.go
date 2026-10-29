package messages

import (
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"

	"github.com/docker/docker-agent/pkg/tui/widgets/key"
)

func TestMessageKeyBindingsKeepKeySets(t *testing.T) {
	t.Parallel()
	assert.Equal(t, []string{"c"}, messageKeys.Copy.Keys())
	assert.Equal(t, []string{"p"}, messageKeys.FirstAttachment.Keys())
	assert.Equal(t, []string{"e"}, messageKeys.Edit.Keys())
	assert.False(t, key.Matches(tea.KeyPressMsg{Code: 'x', Text: "x"}, messageKeys.Copy, messageKeys.Edit))
}

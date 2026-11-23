package main

import (
	"testing"

	"github.com/dgageot/rubocop-go/coptest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestConfigLatestYAMLOmitzero(t *testing.T) {
	t.Parallel()
	src := "package latest\ntype Config struct {\n Good map[string]any `json:\"good,omitzero\" yaml:\"good,omitzero\"`\n Bad map[string]any `json:\"bad\" yaml:\"bad,omitzero\"`\n}"
	offenses := coptest.RunNamed(t, ConfigLatestTagConsistency, "pkg/config/latest/types.go", src)
	require.Len(t, offenses, 1)
	assert.Contains(t, offenses[0].Message, "field Bad")
}

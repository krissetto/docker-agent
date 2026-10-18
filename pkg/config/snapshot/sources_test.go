package snapshot_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/config/snapshot"
)

func TestManifestSourcesReturnsDeterministicDefensiveDescriptors(t *testing.T) {
	manifest, err := snapshot.Snapshot(t.Context(), config.NewBytesSource("root.yaml", []byte(simpleYAML)), snapshot.Options{Startup: snapshot.Startup{Workspace: t.TempDir(), SourceKey: "root"}})
	require.NoError(t, err)
	first := manifest.Sources()
	require.Len(t, first, 1)
	assert.Equal(t, "$root", first[0].Key)
	assert.Equal(t, "root.yaml", first[0].Name)
	assert.NotEmpty(t, first[0].ContentDigest)
	first[0].Name, first[0].References = "mutated", []string{"mutated"}
	second := manifest.Sources()
	assert.Equal(t, "root.yaml", second[0].Name)
	assert.Empty(t, second[0].References)
}

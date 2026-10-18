package teamdef

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config"
)

func TestEditableSourceCapturesImmutableRevisionAndRejectsUnsupportedDocuments(t *testing.T) {
	t.Parallel()
	yaml := `version: "16"
# Team comments survive a controlled patch.
agents:
  root:
    model: openai/root
    instruction: "${UNRESOLVED_PROMPT}"
`
	source := config.NewBytesSource("team.yaml", []byte(yaml))
	editable, err := NewEditableSource(t.Context(), source)
	require.NoError(t, err)
	revision := editable.CurrentRevision()
	loaded, err := editable.SourceForLoad(revision)
	require.NoError(t, err)
	bytes, err := loaded.Read(t.Context())
	require.NoError(t, err)
	bytes[0] = '!'
	second, err := loaded.Read(t.Context())
	require.NoError(t, err)
	assert.YAMLEq(t, yaml, string(second), "each read must have independent bytes")
	assert.NotContains(t, revision.ID, "${UNRESOLVED_PROMPT}")

	for _, tc := range []struct{ name, yaml, want string }{
		{"old version", "version: '15'\nagents: {root: {model: 'openai/x'}}\n", "version 16 only"},
		{"flavors", "flavors: {}\nagents: {root: {model: 'openai/x'}}\n", "flavors"},
		{"include", "include: other.yaml\nagents: {root: {model: 'openai/x'}}\n", "include"},
		{"anchor", "agents: {root: &a {model: 'openai/x'}}\n", "aliases or anchors"},
		{"shared source", "agents: {root: {model: 'openai/x', use_toolsets: [shared]}}\n", "use_toolsets"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewEditableSource(t.Context(), config.NewBytesSource("team.yaml", []byte(tc.yaml)))
			require.ErrorContains(t, err, tc.want)
		})
	}
}

func TestEditableSourcePersistenceConflictAndExclusiveExport(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "team.yaml")
	original := []byte("agents:\n  root:\n    instruction: original\n")
	require.NoError(t, os.WriteFile(path, original, 0o640))
	source, err := NewEditableSourceWithOptions(t.Context(), config.NewFileSource(path), EditableSourceOptions{ExportRoot: dir, WritablePath: path})
	require.NoError(t, err)
	base := source.CurrentRevision()
	candidate := []byte("agents:\n  root:\n    instruction: edited\n")
	newRevision, err := source.commit(base, candidate)
	require.NoError(t, err)
	require.NoError(t, source.Persist(newRevision))
	got, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, candidate, got)
	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o640), info.Mode().Perm())

	export := filepath.Join(dir, "export.yaml")
	require.NoError(t, source.ExportNew(newRevision, export))
	info, err = os.Stat(export)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	require.Error(t, source.ExportNew(newRevision, export), "export must not overwrite")

	conflict, err := NewEditableSourceWithOptions(t.Context(), config.NewFileSource(path), EditableSourceOptions{WritablePath: path})
	require.NoError(t, err)
	conflictRevision, err := conflict.commit(conflict.CurrentRevision(), []byte("agents:\n  root:\n    instruction: editor\n"))
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, []byte("agents:\n  root:\n    instruction: outside\n"), 0o640))
	err = conflict.Persist(conflictRevision)
	require.ErrorContains(t, err, "changed externally")
	got, err = os.ReadFile(path)
	require.NoError(t, err)
	assert.Contains(t, string(got), "outside", "external content must remain untouched")
}

func TestEditableSourceRejectsSymlinkForPersistence(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	target := filepath.Join(dir, "target.yaml")
	link := filepath.Join(dir, "team.yaml")
	require.NoError(t, os.WriteFile(target, []byte("agents:\n  root: {instruction: safe}\n"), 0o600))
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	_, err := NewEditableSource(t.Context(), config.NewFileSource(link))
	require.Error(t, err)
}

type namedSource struct {
	name, parent string
	data         []byte
}

func (s namedSource) Name() string      { return s.name }
func (s namedSource) ParentDir() string { return s.parent }
func (s namedSource) Read(context.Context) ([]byte, error) {
	return append([]byte(nil), s.data...), nil
}

func TestEditableSourceRejectsPathEscape(t *testing.T) {
	t.Parallel()
	_, err := NewEditableSource(t.Context(), namedSource{name: "../escape.yaml", parent: t.TempDir(), data: []byte("agents: {root: {instruction: safe}}\n")})
	require.ErrorContains(t, err, "direct source path within its declared directory")
}

func TestImmutableRemoteSourceCanOnlyExportUnderExplicitRoot(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	data := []byte("version: \"16\"\nagents: {root: {instruction: authored}}\n")
	source := namedSource{name: "https://example.invalid/team.yaml", data: data}
	editable, err := NewEditableSourceWithExportRoot(t.Context(), source, dir)
	require.NoError(t, err)
	require.ErrorContains(t, editable.Persist(editable.CurrentRevision()), "no original writable local file")
	export := filepath.Join(dir, "copy.yaml")
	require.NoError(t, editable.ExportNew(editable.CurrentRevision(), export))
	bytes, err := os.ReadFile(export)
	require.NoError(t, err)
	assert.Equal(t, data, bytes)
	outside := filepath.Join(t.TempDir(), "outside.yaml")
	require.ErrorContains(t, editable.ExportNew(editable.CurrentRevision(), outside), "inside the configured export root")
}

func TestEditableSourceBoundsInMemoryRevisionHistory(t *testing.T) {
	t.Parallel()
	source, err := NewEditableSource(t.Context(), config.NewBytesSource("team.yaml", []byte("agents: {root: {instruction: one}}\n")))
	require.NoError(t, err)
	first := source.CurrentRevision()
	for i := range maxEditableRevisions + 2 {
		current := source.CurrentRevision()
		_, err := source.commit(current, []byte(fmt.Sprintf("agents: {root: {instruction: %d}}\n", i)))
		require.NoError(t, err)
	}
	_, err = source.SourceForLoad(first)
	require.ErrorContains(t, err, "unknown editable source revision")
	assert.LessOrEqual(t, len(source.revisions), maxEditableRevisions)
}

func TestEditableSourceNeedsExplicitWritableCapability(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "team.yaml")
	require.NoError(t, os.WriteFile(path, []byte("agents: {root: {instruction: authored}}\n"), 0o600))
	editable, err := NewEditableSource(t.Context(), config.NewFileSource(path))
	require.NoError(t, err)
	require.ErrorContains(t, editable.Persist(editable.CurrentRevision()), "no original writable local file")
	_, err = NewEditableSourceWithOptions(t.Context(), config.NewBytesSource(path, []byte("agents: {root: {instruction: authored}}\n")), EditableSourceOptions{WritablePath: path})
	require.Error(t, err)
}

package teamdef

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config"
)

func TestEditorPersistCurrentUsesExactCASAndTrustedManagedCallback(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "team.yaml")
	original := []byte("version: \"16\"\nagents:\n  root:\n    instruction: before\n")
	require.NoError(t, os.WriteFile(path, original, 0o600))
	editor, err := NewEditor(t.Context(), EditableSourceOptions{
		Source:       config.NewFileSource(path),
		Authority:    EditAuthorityFull,
		WritablePath: path,
		BuildValidator: func(ctx context.Context, source config.Source) error {
			_, err := config.Load(ctx, source)
			return err
		},
	})
	require.NoError(t, err)
	base := editor.CurrentRevision()
	actor := testActor(editor, "root")
	result, err := editor.Replace(t.Context(), base, actor, []byte("version: \"16\"\nagents:\n  root:\n    instruction: after\n"), false)
	require.NoError(t, err)
	var committed []byte
	err = editor.PersistCurrent(t.Context(), result.Revision, func(ctx context.Context, source config.Source) error {
		data, err := source.Read(ctx)
		committed = data
		return err
	})
	require.NoError(t, err)
	require.Contains(t, string(committed), "instruction: after")
	persisted, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, original, persisted, "managed callback is used instead of local writable path")
	err = editor.PersistCurrent(t.Context(), base, func(context.Context, config.Source) error {
		t.Fatal("stale revision must not invoke persistence")
		return nil
	})
	require.ErrorContains(t, err, "revision conflict")
}

func TestEditorPersistCurrentRejectsMissingLocalCapability(t *testing.T) {
	editor, err := NewEditor(t.Context(), EditableSourceOptions{
		Source:         config.NewBytesSource("team.yaml", []byte("version: \"16\"\nagents: {root: {instruction: safe}}\n")),
		Authority:      EditAuthorityFull,
		BuildValidator: func(context.Context, config.Source) error { return nil },
	})
	require.NoError(t, err)
	require.ErrorContains(t, editor.PersistCurrent(t.Context(), editor.CurrentRevision(), nil), "no original writable local file")
}

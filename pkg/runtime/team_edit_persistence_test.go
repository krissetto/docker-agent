package runtime

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/config"
	"github.com/docker/docker-agent/pkg/teamdef"
)

func persistenceTestEditor(t *testing.T, path string, exportRoot string) *teamdef.Editor {
	t.Helper()
	editor, err := teamdef.NewEditor(t.Context(), teamdef.EditableSourceOptions{
		Source:       config.NewFileSource(path),
		Authority:    teamdef.EditAuthorityFull,
		WritablePath: path,
		ExportRoot:   exportRoot,
		BuildValidator: func(ctx context.Context, source config.Source) error {
			_, err := config.Load(ctx, source)
			return err
		},
	})
	require.NoError(t, err)
	return editor
}

func TestPersistTeamEditSourceUpdatesManagedCASOnlyAfterCommit(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "team.yaml")
	require.NoError(t, os.WriteFile(path, []byte("version: \"16\"\nagents: {root: {instruction: start}}\n"), 0o600))
	editor := persistenceTestEditor(t, path, dir)
	base := editor.CurrentRevision()
	result, err := editor.Replace(t.Context(), base, teamdef.ActorRef{TeamID: editor.TeamID(), AgentKey: "root", SourceRevision: editor.BaselineRevisionID()}, []byte("version: \"16\"\nagents: {root: {instruction: edited}}\n"), false)
	require.NoError(t, err)
	var gotCandidate string
	persistence := &TeamEditPersistence{ExpectedSourceRevision: "managed-r1", Persist: func(ctx context.Context, expected string, candidate config.Source) (string, error) {
		require.Equal(t, "managed-r1", expected)
		data, err := candidate.Read(ctx)
		gotCandidate = string(data)
		return "managed-r2", err
	}}
	receipt, err := persistTeamEditSource(t.Context(), editor, result.Revision, persistence)
	require.NoError(t, err)
	require.True(t, receipt.Persisted)
	require.Equal(t, "managed-r2", persistence.ExpectedSourceRevision)
	require.Contains(t, gotCandidate, "instruction: edited")
	_, err = persistTeamEditSource(t.Context(), editor, base, persistence)
	require.ErrorContains(t, err, "revision conflict")
	require.Equal(t, "managed-r2", persistence.ExpectedSourceRevision)
}

func TestPersistTeamEditSourceLeavesCASOnCommitFailure(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "team.yaml")
	require.NoError(t, os.WriteFile(path, []byte("version: \"16\"\nagents: {root: {instruction: start}}\n"), 0o600))
	editor := persistenceTestEditor(t, path, dir)
	persistence := &TeamEditPersistence{ExpectedSourceRevision: "managed-r1", Persist: func(context.Context, string, config.Source) (string, error) {
		return "", errors.New("external conflict")
	}}
	_, err := persistTeamEditSource(t.Context(), editor, editor.CurrentRevision(), persistence)
	require.ErrorContains(t, err, "external conflict")
	require.Equal(t, "managed-r1", persistence.ExpectedSourceRevision)
}

func TestExportTeamEditSourceUsesTrustedNoClobberCapability(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "team.yaml")
	require.NoError(t, os.WriteFile(path, []byte("version: \"16\"\nagents: {root: {instruction: safe}}\n"), 0o600))
	editor := persistenceTestEditor(t, path, dir)
	destination := filepath.Join(dir, "export.yaml")
	receipt, err := exportTeamEditSource(editor, editor.CurrentRevision(), destination)
	require.NoError(t, err)
	require.True(t, receipt.Exported)
	_, err = exportTeamEditSource(editor, editor.CurrentRevision(), destination)
	require.Error(t, err)
	got, err := os.ReadFile(destination)
	require.NoError(t, err)
	require.Contains(t, string(got), "instruction: safe")
}

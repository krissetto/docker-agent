package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestClone_ChangeWorkingDir(t *testing.T) {
	t.Parallel()
	original := &RuntimeConfig{
		Config: Config{
			EnvFiles:       []string{"file1.env", "file2.env"},
			ModelsGateway:  "http://models.gateway",
			GlobalCodeMode: true,
			WorkingDir:     "/app",
		},
	}

	clone := original.Clone()
	original.WorkingDir = "/newapp"
	clone.WorkingDir = "/cloneapp"

	assert.Equal(t, "/newapp", original.WorkingDir)
	assert.Equal(t, "/cloneapp", clone.WorkingDir)
}

func TestEnvFilesError(t *testing.T) {
	t.Parallel()

	t.Run("missing file", func(t *testing.T) {
		t.Parallel()
		missing := filepath.Join(t.TempDir(), "missing.env")
		rc := &RuntimeConfig{Config: Config{EnvFiles: []string{missing}}}

		err := rc.EnvFilesError()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "missing.env")
	})

	t.Run("malformed file", func(t *testing.T) {
		t.Parallel()
		bad := filepath.Join(t.TempDir(), "bad.env")
		require.NoError(t, os.WriteFile(bad, []byte("NOT_A_PAIR\n"), 0o600))
		rc := &RuntimeConfig{Config: Config{EnvFiles: []string{bad}}}

		err := rc.EnvFilesError()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "bad.env")
	})

	t.Run("valid file", func(t *testing.T) {
		t.Parallel()
		ok := filepath.Join(t.TempDir(), "ok.env")
		require.NoError(t, os.WriteFile(ok, []byte("SOME_TEST_ONLY_VAR=some-value\n"), 0o600))
		rc := &RuntimeConfig{Config: Config{EnvFiles: []string{ok}}}

		require.NoError(t, rc.EnvFilesError())
		v, _ := rc.EnvProvider().Get(t.Context(), "SOME_TEST_ONLY_VAR")
		assert.Equal(t, "some-value", v)
	})

	t.Run("clone preserves error", func(t *testing.T) {
		t.Parallel()
		missing := filepath.Join(t.TempDir(), "missing.env")
		rc := &RuntimeConfig{Config: Config{EnvFiles: []string{missing}}}

		require.Error(t, rc.Clone().EnvFilesError())
	})
}

func TestCloneCopiesTeamEditGrantAndPreservesNilAllowlist(t *testing.T) {
	t.Parallel()
	original := &RuntimeConfig{Config: Config{TeamEditMode: "full", TeamEditAllowedAgentIDs: nil}}
	clone := original.Clone()
	require.Equal(t, "full", clone.TeamEditGrant().Mode)
	require.Nil(t, clone.TeamEditGrant().AllowedAgentIDs, "nil means trusted source-wide authority")

	original.TeamEditAllowedAgentIDs = []string{"root"}
	clone = original.Clone()
	original.TeamEditAllowedAgentIDs[0] = "changed"
	require.Equal(t, []string{"root"}, clone.TeamEditGrant().AllowedAgentIDs)
}

func TestValidateTeamEditGrantModesAndAllowlist(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		mode string
		ids  []string
		want string
	}{
		{"", nil, ""},
		{"off", nil, ""},
		{"restricted", []string{"root"}, ""},
		{"full", nil, ""},
		{"off", []string{}, "requires --team-edit"},
		{"sometimes", nil, "expected off, restricted, or full"},
		{"full", []string{"root", "root"}, "unique"},
		{"full", []string{" root"}, "non-empty and trimmed"},
	} {
		t.Run(tc.mode+"/"+tc.want, func(t *testing.T) {
			run := &RuntimeConfig{Config: Config{TeamEditMode: tc.mode, TeamEditAllowedAgentIDs: tc.ids}}
			err := run.ValidateTeamEditGrant()
			if tc.want == "" {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, tc.want)
			}
		})
	}
}

func TestCloneTeamEditNilAndExplicitAllowlistSemantics(t *testing.T) {
	t.Parallel()
	sourceWide := (&RuntimeConfig{Config: Config{TeamEditMode: "full"}}).Clone().TeamEditGrant()
	require.Nil(t, sourceWide.AllowedAgentIDs)

	explicitEmpty := (&RuntimeConfig{Config: Config{TeamEditMode: "restricted", TeamEditAllowedAgentIDs: []string{}}}).Clone().TeamEditGrant()
	require.NotNil(t, explicitEmpty.AllowedAgentIDs)
	require.Empty(t, explicitEmpty.AllowedAgentIDs)
}

func TestTeamEditGrantRuntimeJSONRoundTripAndDefaultDigestStability(t *testing.T) {
	t.Parallel()
	defaultConfig := RuntimeConfig{Config: Config{WorkingDir: "/tmp"}}
	require.NoError(t, defaultConfig.ValidateTeamEditGrant())
	require.Empty(t, defaultConfig.TeamEditMode, "default off canonicalizes to the old zero value")
	wide := RuntimeConfig{Config: Config{TeamEditMode: "full"}}
	require.NoError(t, wide.ValidateTeamEditGrant())
	data, err := json.Marshal(wide.Config)
	require.NoError(t, err)
	var roundTrip Config
	require.NoError(t, json.Unmarshal(data, &roundTrip))
	require.Equal(t, "full", roundTrip.TeamEditMode)
	denyAll := RuntimeConfig{Config: Config{TeamEditMode: "restricted", TeamEditAllowedAgentIDsSet: true, TeamEditAllowedAgentIDs: []string{}}}
	require.NoError(t, denyAll.ValidateTeamEditGrant())
	clonedGrant := denyAll.Clone().TeamEditGrant()
	require.True(t, clonedGrant.AllowedAgentIDsSet)
	require.Empty(t, clonedGrant.AllowedAgentIDs)
}

func TestTeamEditDefaultOffDoesNotChangeLegacyConfigJSON(t *testing.T) {
	t.Parallel()
	before := Config{WorkingDir: "/tmp"}
	after := before
	runtimeConfig := RuntimeConfig{Config: after}
	require.NoError(t, runtimeConfig.ValidateTeamEditGrant())
	beforeJSON, err := json.Marshal(before)
	require.NoError(t, err)
	afterJSON, err := json.Marshal(runtimeConfig.Config)
	require.NoError(t, err)
	assert.JSONEq(t, string(beforeJSON), string(afterJSON), "default off should be omitted for old snapshots")
}

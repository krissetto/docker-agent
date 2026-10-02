package root

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/paths"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/selfupdate"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/session/sqlitestore"
)

func isolateSessionsList(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv(envConfigDir, "")
	t.Setenv(cagentEnvConfigDir, "")
	t.Setenv("CAGENT_HIDE_TELEMETRY_BANNER", "")
	t.Setenv("DOCKER_AGENT_HIDE_TELEMETRY_BANNER", "")
	paths.SetRoot("")
	t.Cleanup(func() { paths.SetRoot("") })
	return home
}

func TestSessionsListQuiet(t *testing.T) {
	for _, location := range []string{"default", "data-dir", "session-db", "home-expansion"} {
		t.Run(location, func(t *testing.T) {
			home := isolateSessionsList(t)
			configDir := filepath.Join(home, "custom-config")
			args := []string{"--config-dir", configDir, "sessions", "list", "--quiet"}
			dbPath := filepath.Join(home, ".cagent", "session.db")
			switch location {
			case "data-dir":
				dbPath = filepath.Join(home, "custom-data", "session.db")
				args = append(args, "--data-dir", filepath.Dir(dbPath))
			case "session-db":
				dbPath = filepath.Join(home, "explicit.db")
				args = append(args, "--data-dir", filepath.Join(home, "unused"), "--session-db", dbPath)
			case "home-expansion":
				dbPath = filepath.Join(home, "explicit.db")
				args = append(args, "--session-db", "~/explicit.db")
			}
			store, err := sqlitestore.New(t.Context(), dbPath)
			require.NoError(t, err)
			older := session.New(session.WithUserMessage("older persisted message"))
			older.CreatedAt = time.Now().Add(-time.Hour)
			newer := session.New(session.WithUserMessage("newer persisted message"))
			child := session.New(session.WithParentID(newer.ID), session.WithAsyncSubagent(true))
			for _, sess := range []*session.Session{older, newer, child} {
				require.NoError(t, store.AddSession(t.Context(), sess))
			}
			require.NoError(t, store.Close())
			var stdout, stderr bytes.Buffer
			cmd := NewRootCmd()
			cmd.SetOut(&stdout)
			cmd.SetErr(&stderr)
			cmd.SetArgs(args)
			require.NoError(t, cmd.ExecuteContext(t.Context()))
			assert.Equal(t, newer.ID+"\n"+older.ID+"\n", stdout.String())
			assert.Empty(t, stderr.String())
			assert.Equal(t, configDir, paths.GetConfigDir())
			assert.NoDirExists(t, configDir, "listing must not create first-run state")
			assert.NoDirExists(t, filepath.Join(home, "unused"))

			store, err = sqlitestore.New(t.Context(), dbPath)
			require.NoError(t, err)
			defer store.Close()
			for _, id := range strings.Fields(stdout.String()) {
				f := &runExecFlags{}
				rt, resumed, err := f.createLocalRuntimeAndSession(t.Context(), newSessionTestLoadResult(), runtime.CreateSessionRequest{AgentName: "root", ResumeSessionID: id}, store)
				require.NoError(t, err)
				assert.Equal(t, id, resumed.ID)
				require.Len(t, resumed.Messages, 1, "resume must load existing history, not create a fresh session")
				require.NoError(t, rt.Close())
			}
		})
	}
}

func TestSessionsListQuietEmpty(t *testing.T) {
	for _, existing := range []bool{false, true} {
		t.Run(map[bool]string{false: "missing", true: "empty"}[existing], func(t *testing.T) {
			home := isolateSessionsList(t)
			dbPath := filepath.Join(home, "data", "session.db")
			if existing {
				store, err := sqlitestore.New(t.Context(), dbPath)
				require.NoError(t, err)
				require.NoError(t, store.Close())
			}
			var stdout, stderr bytes.Buffer
			cmd := NewRootCmd()
			cmd.SetOut(&stdout)
			cmd.SetErr(&stderr)
			cmd.SetArgs([]string{"sessions", "list", "-q", "--data-dir", filepath.Dir(dbPath)})
			require.NoError(t, cmd.ExecuteContext(t.Context()))
			assert.Empty(t, stdout.String())
			assert.Empty(t, stderr.String())
			if !existing {
				assert.NoDirExists(t, filepath.Dir(dbPath))
			}
		})
	}
}

func TestSessionsListExecuteSkipsSelfUpdate(t *testing.T) {
	home := isolateSessionsList(t)
	t.Setenv(selfupdate.EnvAutoUpdate, "true")
	backup := filepath.Join(home, ".docker-agent-backup-test")
	require.NoError(t, os.WriteFile(backup, []byte("preserved"), 0o600))
	t.Setenv("DOCKER_AGENT_SELF_UPDATE_BACKUP", backup)
	var stdout, stderr bytes.Buffer
	require.NoError(t, Execute(t.Context(), strings.NewReader(""), &stdout, &stderr,
		"--data-dir", filepath.Join(home, "data"), "sessions", "list", "--quiet"))
	assert.Empty(t, stdout.String())
	assert.Empty(t, stderr.String())
	assert.FileExists(t, backup)
}

func TestSessionsListQuietError(t *testing.T) {
	home := isolateSessionsList(t)
	path := filepath.Join(home, "corrupt.db")
	require.NoError(t, os.WriteFile(path, []byte("not a database"), 0o600))
	var stdout, stderr bytes.Buffer
	cmd := NewRootCmd()
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"sessions", "list", "--quiet", "--session-db", path})
	require.Error(t, cmd.ExecuteContext(t.Context()))
	assert.Empty(t, stdout.String())
	assert.Contains(t, stderr.String(), "Error: listing sessions:")
	assert.NotContains(t, stderr.String(), "Welcome")
	assert.NotContains(t, stderr.String(), "Usage:")
	assert.NoFileExists(t, path+".bak")
}

func TestSessionsListHumanOutput(t *testing.T) {
	home := isolateSessionsList(t)
	var stdout, stderr bytes.Buffer
	cmd := NewRootCmd()
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"sessions", "list", "--session-db", filepath.Join(home, "missing.db")})
	require.NoError(t, cmd.ExecuteContext(t.Context()))
	assert.Equal(t, "SESSION ID\n", stdout.String())
	assert.Empty(t, stderr.String())
}

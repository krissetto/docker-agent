package board

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/paths"
	"github.com/docker/docker-agent/pkg/userconfig"
)

func TestNormalizeProjectPath(t *testing.T) {
	input := filepath.Join(t.TempDir(), "repo")
	abs, err := normalizeProjectPath(input)
	require.NoError(t, err)
	assert.Equal(t, input, abs)

	// Empty and blank paths are rejected: they would silently validate
	// against the board's working directory.
	_, err = normalizeProjectPath("")
	require.Error(t, err)
	_, err = normalizeProjectPath("   ")
	require.Error(t, err)

	// A leading ~ expands to the home directory.
	abs, err = normalizeProjectPath("~/src/repo")
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(paths.GetHomeDir(), "src", "repo"), abs)

	// Relative paths are anchored to the current directory.
	abs, err = normalizeProjectPath("repo")
	require.NoError(t, err)
	assert.True(t, filepath.IsAbs(abs))
}

func TestUpdateProject(t *testing.T) {
	paths.SetConfigDir(t.TempDir())
	t.Cleanup(func() { paths.SetConfigDir("") })

	repo := newLocalRepo(t)
	repo2 := newLocalRepo(t)

	cfg, err := userconfig.Load()
	require.NoError(t, err)
	store, err := OpenStore(filepath.Join(t.TempDir(), "cards.json"))
	require.NoError(t, err)
	app := &App{ctx: t.Context(), config: cfg, columns: DefaultColumns, store: store, onChanged: func() {}}

	require.NoError(t, app.AddProject(Project{Name: "one", Path: repo}))
	require.NoError(t, app.AddProject(Project{Name: "two", Path: repo2}))
	require.NoError(t, store.InsertCard(&Card{ID: "card1", Project: "one"}))
	require.NoError(t, store.InsertCard(&Card{ID: "card2", Project: "two"}))

	// Rename, repoint, and set the agent in one update; order is preserved
	// and existing cards follow the rename.
	require.NoError(t, app.UpdateProject("one", Project{Name: "renamed", Path: repo2, Agent: "coder"}))
	projects := app.Projects()
	require.Len(t, projects, 2)
	assert.Equal(t, Project{Name: "renamed", Path: repo2, Agent: "coder"}, projects[0])
	card, err := store.GetCard("card1")
	require.NoError(t, err)
	assert.Equal(t, "renamed", card.Project)
	card, err = store.GetCard("card2")
	require.NoError(t, err)
	assert.Equal(t, "two", card.Project)

	// Unknown project, duplicate name, and AddProject-style validation.
	require.Error(t, app.UpdateProject("one", Project{Name: "x", Path: repo}))
	require.Error(t, app.UpdateProject("renamed", Project{Name: "two", Path: repo}))
	require.Error(t, app.UpdateProject("renamed", Project{Name: "", Path: repo}))
	require.Error(t, app.UpdateProject("renamed", Project{Name: "renamed", Path: ""}))
	require.Error(t, app.UpdateProject("renamed", Project{Name: "renamed", Path: t.TempDir()})) // not a git repo

	// Keeping the name while changing other fields is not a duplicate.
	require.NoError(t, app.UpdateProject("two", Project{Name: "two", Path: repo}))

	// The update is persisted to the config file.
	reloaded, err := userconfig.Load()
	require.NoError(t, err)
	require.Len(t, reloaded.Board.Projects, 2)
	assert.Equal(t, "renamed", reloaded.Board.Projects[0].Name)
	assert.Equal(t, "coder", reloaded.Board.Projects[0].Agent)
}

func TestMoveProject(t *testing.T) {
	paths.SetConfigDir(t.TempDir())
	t.Cleanup(func() { paths.SetConfigDir("") })

	cfg, err := userconfig.Load()
	require.NoError(t, err)
	cfg.Board = &userconfig.Board{Projects: []userconfig.BoardProject{
		{Name: "a", Path: "/a"}, {Name: "b", Path: "/b"}, {Name: "c", Path: "/c"},
	}}
	app := &App{ctx: t.Context(), config: cfg, columns: DefaultColumns}

	names := func() []string {
		var out []string
		for _, p := range app.Projects() {
			out = append(out, p.Name)
		}
		return out
	}

	require.NoError(t, app.MoveProject("c", -1))
	assert.Equal(t, []string{"a", "c", "b"}, names())

	// Moves past either end clamp to a no-op.
	require.NoError(t, app.MoveProject("a", -1))
	require.NoError(t, app.MoveProject("b", 5))
	assert.Equal(t, []string{"a", "c", "b"}, names())

	require.Error(t, app.MoveProject("nope", 1))

	// The new order is persisted to the config file.
	reloaded, err := userconfig.Load()
	require.NoError(t, err)
	assert.Equal(t, "c", reloaded.Board.Projects[1].Name)
}

func TestColumnCRUD(t *testing.T) {
	paths.SetConfigDir(t.TempDir())
	t.Cleanup(func() { paths.SetConfigDir("") })

	cfg, err := userconfig.Load()
	require.NoError(t, err)
	store, err := OpenStore(filepath.Join(t.TempDir(), "cards.json"))
	require.NoError(t, err)
	app := &App{ctx: t.Context(), config: cfg, columns: ColumnsFromConfig(nil), store: store, onChanged: func() {}}

	ids := func() []string {
		var out []string
		for _, c := range app.Columns() {
			out = append(out, c.ID)
		}
		return out
	}

	// Add derives the id from the name and keeps it unique.
	col, err := app.AddColumn(Column{Name: "  QA Check ", Emoji: " \U0001f9ea ", Prompt: "run the tests"})
	require.NoError(t, err)
	assert.Equal(t, Column{ID: "qa-check", Name: "QA Check", Emoji: "\U0001f9ea", Prompt: "run the tests"}, col)
	dup, err := app.AddColumn(Column{Name: "QA Check"})
	require.NoError(t, err)
	assert.Equal(t, "qa-check-2", dup.ID)
	_, err = app.AddColumn(Column{Name: "   "})
	require.Error(t, err)

	// Update keeps the id stable across a rename.
	updated, err := app.UpdateColumn("qa-check", Column{Name: "Verify", Prompt: "run the tests"})
	require.NoError(t, err)
	assert.Equal(t, Column{ID: "qa-check", Name: "Verify", Prompt: "run the tests"}, updated)
	_, err = app.UpdateColumn("nope", Column{Name: "x"})
	require.Error(t, err)
	_, err = app.UpdateColumn("qa-check", Column{Name: ""})
	require.Error(t, err)

	// Move clamps at both ends.
	require.NoError(t, app.MoveColumn("qa-check", -2))
	assert.Equal(t, []string{"dev", "review", "qa-check", "push", "done", "qa-check-2"}, ids())
	require.NoError(t, app.MoveColumn("dev", -1))
	require.NoError(t, app.MoveColumn("qa-check-2", 5))
	assert.Equal(t, []string{"dev", "review", "qa-check", "push", "done", "qa-check-2"}, ids())
	require.Error(t, app.MoveColumn("nope", 1))

	// A column that still has cards cannot be removed.
	require.NoError(t, store.InsertCard(&Card{ID: "c1", Column: "qa-check"}))
	require.Error(t, app.RemoveColumn("qa-check"))
	require.NoError(t, app.RemoveColumn("qa-check-2"))
	require.Error(t, app.RemoveColumn("nope"))
	assert.Equal(t, []string{"dev", "review", "qa-check", "push", "done"}, ids())

	// The customized pipeline is persisted to the config file.
	reloaded, err := userconfig.Load()
	require.NoError(t, err)
	require.NotNil(t, reloaded.Board)
	assert.Equal(t, ids(), func() []string {
		var out []string
		for _, c := range reloaded.Board.Columns {
			out = append(out, c.ID)
		}
		return out
	}())

	// Restoring the defaults drops the columns section from the config, so
	// future default-pipeline changes reach the user again.
	require.NoError(t, store.DeleteCard("c1"))
	require.NoError(t, app.RemoveColumn("qa-check"))
	reloaded, err = userconfig.Load()
	require.NoError(t, err)
	assert.Empty(t, reloaded.Board.Columns)
}

func TestRemoveColumnKeepsAtLeastOne(t *testing.T) {
	paths.SetConfigDir(t.TempDir())
	t.Cleanup(func() { paths.SetConfigDir("") })

	cfg, err := userconfig.Load()
	require.NoError(t, err)
	store, err := OpenStore(filepath.Join(t.TempDir(), "cards.json"))
	require.NoError(t, err)
	app := &App{ctx: t.Context(), config: cfg, columns: []Column{{ID: "only", Name: "Only"}}, store: store, onChanged: func() {}}

	require.Error(t, app.RemoveColumn("only"))
	assert.Len(t, app.Columns(), 1)
}

func TestAttachCommandExplainsFailedRelaunch(t *testing.T) {
	t.Parallel()

	store := testStore(t)
	require.NoError(t, store.InsertCard(&Card{ID: "c1", Status: StatusError, Session: "s", Worktree: t.TempDir()}))

	// The session cannot be recreated: there is no pane holding the error,
	// so attach must report why the relaunch failed instead of a bare
	// "session is gone".
	sessions := &crashingSessions{newErr: errors.New("tmux: bad working directory")}
	c := newController(t.Context(), store, sessions, func() {})
	card, err := store.GetCard("c1")
	require.NoError(t, err)
	c.resume(card)

	app := &App{ctx: t.Context(), store: store, sessions: sessions, controller: c, onChanged: func() {}}
	_, err = app.AttachCommand("c1")
	assert.ErrorContains(t, err, "bad working directory")
}

type stoppingCardClient struct {
	fakeClient

	stopErr error
	stopped bool
}

func (c *stoppingCardClient) StopSubtree(context.Context) error {
	c.stopped = true
	return c.stopErr
}

func TestDeleteCardRequiresAuthorityDrainBeforeRemovingWorktree(t *testing.T) {
	store := testStore(t)
	worktree := t.TempDir()
	marker := filepath.Join(worktree, "keep")
	require.NoError(t, os.WriteFile(marker, []byte("unsettled tools"), 0o600))
	require.NoError(t, store.InsertCard(&Card{ID: "c", Session: "tmux", AgentSession: "session", Worktree: worktree}))
	client := &stoppingCardClient{stopErr: errors.New("authority unavailable")}
	controller := newController(t.Context(), store, fakeSessions{}, func() {})
	controller.clientFor = func(_, _ string) sessionClient { return client }
	app := &App{ctx: t.Context(), store: store, sessions: fakeSessions{}, controller: controller, onChanged: func() {}}
	require.ErrorContains(t, app.DeleteCard("c"), "authority unavailable")
	assert.True(t, client.stopped)
	_, err := store.GetCard("c")
	require.NoError(t, err)
	_, err = os.Stat(marker)
	require.NoError(t, err)
}

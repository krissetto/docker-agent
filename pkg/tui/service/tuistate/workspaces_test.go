package tuistate

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/sqliteutil"
)

func workspaceFixture() WorkspaceState {
	return WorkspaceState{
		Version:  WorkspaceStateVersion,
		ActiveID: "work",
		Workspaces: []Workspace{
			{ID: "empty", Name: "Later"},
			{
				ID: "work", Name: "Project α", FocusedSessionID: "missing-session",
				Sidebar: SidebarState{Collapsed: true, Hidden: true, PreferredWidth: 42},
				Layout: &LayoutNode{
					Axis: SplitColumns, Ratio: 0.35,
					First: &LayoutNode{SessionID: "canonical-session"},
					Second: &LayoutNode{
						Axis: SplitRows, Ratio: 0.6,
						First:  &LayoutNode{SessionID: "other-session"},
						Second: &LayoutNode{SessionID: "missing-session"},
					},
				},
			},
		},
	}
}

func TestWorkspaceRoundTripAndDetachedSnapshots(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	ctx := t.Context()
	_, err := store.GetWorkspaces(ctx)
	require.ErrorIs(t, err, sql.ErrNoRows)

	want := workspaceFixture()
	input := want.Clone()
	require.NoError(t, store.SaveWorkspaces(ctx, input))
	input.Workspaces[1].Name = "mutated"
	input.Workspaces[1].Layout.Second.Second.SessionID = "mutated"
	input.Workspaces[1].Sidebar.PreferredWidth = 1

	got, err := store.GetWorkspaces(ctx)
	require.NoError(t, err)
	assert.Equal(t, want, got)
	got.Workspaces[1].Layout.First.SessionID = "changed after read"
	got.Workspaces[0].Name = "changed after read"
	again, err := store.GetWorkspaces(ctx)
	require.NoError(t, err)
	assert.Equal(t, want, again)

	want.Workspaces[0], want.Workspaces[1] = want.Workspaces[1], want.Workspaces[0]
	want.ActiveID = "empty"
	require.NoError(t, store.SaveWorkspaces(ctx, want))
	got, err = store.GetWorkspaces(ctx)
	require.NoError(t, err)
	assert.Equal(t, want, got)
}

func TestWorkspaceReopen(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "tui_state.db")
	db, err := sqliteutil.OpenDB(t.Context(), path)
	require.NoError(t, err)
	store := &Store{db: db}
	t.Cleanup(func() { _ = store.Close() })
	require.NoError(t, store.SaveWorkspaces(t.Context(), workspaceFixture()))
	require.NoError(t, store.Close())

	db, err = sqliteutil.OpenDB(t.Context(), path)
	require.NoError(t, err)
	reopened := &Store{db: db}
	t.Cleanup(func() { _ = reopened.Close() })
	got, err := reopened.GetWorkspaces(t.Context())
	require.NoError(t, err)
	assert.Equal(t, workspaceFixture(), got)
}

func TestWorkspaceClone(t *testing.T) {
	t.Parallel()
	state := workspaceFixture()
	clone := state.Clone()
	require.Equal(t, state, clone)
	clone.Workspaces[1].Layout.Second.First.SessionID = "changed"
	clone.Workspaces[1].Sidebar.Collapsed = false
	assert.Equal(t, workspaceFixture(), state)
	assert.Nil(t, (WorkspaceState{}).Clone().Workspaces)

	// Invalid cyclic input remains detached and is rejected by validation.
	state.Workspaces[1].Layout.First = state.Workspaces[1].Layout
	clone = state.Clone()
	assert.NotSame(t, state.Workspaces[1].Layout, clone.Workspaces[1].Layout)
	assert.Same(t, clone.Workspaces[1].Layout, clone.Workspaces[1].Layout.First)
	assert.Error(t, clone.Validate())
}

func TestWorkspaceEmptyAndDuplicateNames(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	states := []WorkspaceState{
		{Version: WorkspaceStateVersion},
		{Version: WorkspaceStateVersion, ActiveID: "one", Workspaces: []Workspace{{ID: "one", Name: "Empty"}}},
		{Version: WorkspaceStateVersion, ActiveID: "two", Workspaces: []Workspace{{ID: "one", Name: "Same"}, {ID: "two", Name: "Same"}}},
	}
	for _, state := range states {
		require.NoError(t, store.SaveWorkspaces(t.Context(), state))
		got, err := store.GetWorkspaces(t.Context())
		require.NoError(t, err)
		assert.Equal(t, state, got)
	}
}

func TestWorkspaceInvalidStateIsNotOverwritten(t *testing.T) {
	t.Parallel()
	for name, data := range map[string]string{
		"malformed":       "{broken",
		"missing version": `{"active_id":"","workspaces":[]}`,
		"future version":  `{"version":99,"active_id":"","workspaces":[]}`,
		"null":            "null",
		"trailing":        `{"version":1} {"version":1}`,
		"unknown field":   `{"version":1,"draft":"do not store"}`,
		"invalid active":  `{"version":1,"active_id":"missing"}`,
		"too large":       strings.Repeat(" ", maxWorkspaceBytes+1),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			store := newTestStore(t)
			require.NoError(t, store.ensureWorkspaces(t.Context()))
			_, err := store.db.ExecContext(t.Context(), `INSERT INTO workspace_state (id, state) VALUES (1, ?)`, data)
			require.NoError(t, err)
			_, err = store.GetWorkspaces(t.Context())
			require.Error(t, err)
			require.Error(t, store.SaveWorkspaces(t.Context(), workspaceFixture()))
			var preserved string
			require.NoError(t, store.db.QueryRowContext(t.Context(), `SELECT state FROM workspace_state WHERE id = 1`).Scan(&preserved))
			assert.Equal(t, data, preserved)
		})
	}
}

func TestWorkspaceWriteFailurePreservesState(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	ctx := t.Context()
	original := workspaceFixture()
	require.NoError(t, store.SaveWorkspaces(ctx, original))
	_, err := store.db.ExecContext(ctx, `
		CREATE TRIGGER fail_workspace_update BEFORE UPDATE ON workspace_state
		BEGIN SELECT RAISE(ABORT, 'test write failure'); END
	`)
	require.NoError(t, err)
	changed := original.Clone()
	changed.Workspaces[0].Name = "new name"
	require.ErrorContains(t, store.SaveWorkspaces(ctx, changed), "test write failure")
	got, err := store.GetWorkspaces(ctx)
	require.NoError(t, err)
	assert.Equal(t, original, got)
}

func TestWorkspaceStoreErrors(t *testing.T) {
	t.Parallel()
	t.Run("closed", func(t *testing.T) {
		store := newTestStore(t)
		require.NoError(t, store.Close())
		require.Error(t, store.SaveWorkspaces(t.Context(), workspaceFixture()))
		_, err := store.GetWorkspaces(t.Context())
		require.Error(t, err)
	})
	t.Run("canceled", func(t *testing.T) {
		store := newTestStore(t)
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		require.ErrorIs(t, store.SaveWorkspaces(ctx, workspaceFixture()), context.Canceled)
		_, err := store.GetWorkspaces(ctx)
		require.ErrorIs(t, err, context.Canceled)
	})
}

func TestWorkspaceValidation(t *testing.T) {
	t.Parallel()
	tests := map[string]func(*WorkspaceState){
		"missing version": func(s *WorkspaceState) { s.Version = 0 },
		"unknown version": func(s *WorkspaceState) { s.Version++ },
		"empty ID":        func(s *WorkspaceState) { s.Workspaces[0].ID = " " },
		"empty name":      func(s *WorkspaceState) { s.Workspaces[0].Name = "\t" },
		"duplicate ID":    func(s *WorkspaceState) { s.Workspaces[0].ID = s.Workspaces[1].ID },
		"missing active":  func(s *WorkspaceState) { s.ActiveID = "missing" },
		"empty active":    func(s *WorkspaceState) { s.ActiveID = "" },
		"invalid focus":   func(s *WorkspaceState) { s.Workspaces[1].FocusedSessionID = "missing" },
		"empty focus":     func(s *WorkspaceState) { s.Workspaces[0].FocusedSessionID = "canonical-session" },
		"negative width":  func(s *WorkspaceState) { s.Workspaces[0].Sidebar.PreferredWidth = -1 },
		"duplicate session": func(s *WorkspaceState) {
			s.Workspaces[0].Layout = &LayoutNode{SessionID: "canonical-session"}
		},
		"duplicate local session": func(s *WorkspaceState) {
			s.Workspaces[1].Layout.Second.First.SessionID = "canonical-session"
		},
		"leaf without session": func(s *WorkspaceState) { s.Workspaces[1].Layout.First.SessionID = "" },
		"leaf with axis":       func(s *WorkspaceState) { s.Workspaces[1].Layout.First.Axis = SplitRows },
		"leaf with ratio":      func(s *WorkspaceState) { s.Workspaces[1].Layout.First.Ratio = 0.2 },
		"one child":            func(s *WorkspaceState) { s.Workspaces[1].Layout.First = nil },
		"split with session":   func(s *WorkspaceState) { s.Workspaces[1].Layout.SessionID = "bad" },
		"unknown axis":         func(s *WorkspaceState) { s.Workspaces[1].Layout.Axis = "diagonal" },
		"zero ratio":           func(s *WorkspaceState) { s.Workspaces[1].Layout.Ratio = 0 },
		"one ratio":            func(s *WorkspaceState) { s.Workspaces[1].Layout.Ratio = 1 },
		"negative ratio":       func(s *WorkspaceState) { s.Workspaces[1].Layout.Ratio = -0.1 },
		"large ratio":          func(s *WorkspaceState) { s.Workspaces[1].Layout.Ratio = 1.1 },
		"NaN ratio":            func(s *WorkspaceState) { s.Workspaces[1].Layout.Ratio = math.NaN() },
		"infinite ratio":       func(s *WorkspaceState) { s.Workspaces[1].Layout.Ratio = math.Inf(1) },
		"cycle":                func(s *WorkspaceState) { s.Workspaces[1].Layout.First = s.Workspaces[1].Layout },
		"shared node":          func(s *WorkspaceState) { s.Workspaces[1].Layout.First = s.Workspaces[1].Layout.Second },
		"too deep": func(s *WorkspaceState) {
			for i := range maxWorkspaceDepth {
				s.Workspaces[1].Layout = &LayoutNode{Axis: SplitRows, Ratio: 0.5,
					First: &LayoutNode{SessionID: fmt.Sprintf("depth-%d", i)}, Second: s.Workspaces[1].Layout}
			}
		},
		"too many nodes": func(s *WorkspaceState) {
			id := 0
			var tree func(int) *LayoutNode
			tree = func(depth int) *LayoutNode {
				if depth == 0 {
					id++
					return &LayoutNode{SessionID: fmt.Sprintf("leaf-%d", id)}
				}
				return &LayoutNode{Axis: SplitColumns, Ratio: 0.5, First: tree(depth - 1), Second: tree(depth - 1)}
			}
			s.Workspaces[0].Layout = tree(12)
		},
		"too many workspaces":       func(s *WorkspaceState) { s.Workspaces = make([]Workspace, maxWorkspaceNodes+1) },
		"active without workspaces": func(s *WorkspaceState) { s.Workspaces = nil },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			state := workspaceFixture()
			mutate(&state)
			require.Error(t, state.Validate())
			store := newTestStore(t)
			require.NoError(t, store.SaveWorkspaces(t.Context(), workspaceFixture()))
			require.Error(t, store.SaveWorkspaces(t.Context(), state))
			got, err := store.GetWorkspaces(t.Context())
			require.NoError(t, err)
			assert.Equal(t, workspaceFixture(), got)
		})
	}
}

func TestWorkspaceArrangementsIndependentOfLegacyTabs(t *testing.T) {
	t.Parallel()
	store := newTestStore(t)
	ctx := t.Context()
	require.NoError(t, store.AddTab(ctx, "canonical-session", "/project"))
	require.NoError(t, store.SaveWorkspaces(ctx, workspaceFixture()))
	require.NoError(t, store.ClearTabs(ctx))
	got, err := store.GetWorkspaces(ctx)
	require.NoError(t, err)
	assert.Equal(t, workspaceFixture(), got)
}

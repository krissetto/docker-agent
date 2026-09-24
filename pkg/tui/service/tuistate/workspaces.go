package tuistate

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"strings"
)

const WorkspaceStateVersion = 1

const (
	maxWorkspaceDepth = 64
	maxWorkspaceNodes = 4096
	maxWorkspaceBytes = 4 << 20
)

type SplitAxis string

const (
	SplitColumns SplitAxis = "columns"
	SplitRows    SplitAxis = "rows"
)

// LayoutNode leaves identify persisted sessions, never runtime routes. Missing
// sessions retain their leaves so a failed restore cannot erase an arrangement.
type LayoutNode struct {
	SessionID string      `json:"session_id,omitempty"`
	Axis      SplitAxis   `json:"axis,omitempty"`
	Ratio     float64     `json:"ratio,omitempty"`
	First     *LayoutNode `json:"first,omitempty"`
	Second    *LayoutNode `json:"second,omitempty"`
}

// SidebarState stores user intent, not the layout computed for a terminal size.
type SidebarState struct {
	Collapsed      bool `json:"collapsed,omitempty"`
	Hidden         bool `json:"hidden,omitempty"`
	PreferredWidth int  `json:"preferred_width,omitempty"`
}

type Workspace struct {
	ID               string       `json:"id"`
	Name             string       `json:"name"`
	Layout           *LayoutNode  `json:"layout,omitempty"`
	FocusedSessionID string       `json:"focused_session_id,omitempty"`
	Sidebar          SidebarState `json:"sidebar"`
}

// WorkspaceState contains presentation only, with workspaces in display order.
type WorkspaceState struct {
	Version    int         `json:"version"`
	ActiveID   string      `json:"active_id"`
	Workspaces []Workspace `json:"workspaces"`
}

// Clone returns a detached snapshot, including every layout node.
func (s WorkspaceState) Clone() WorkspaceState {
	cloned := s
	if s.Workspaces == nil {
		return cloned
	}
	cloned.Workspaces = append([]Workspace{}, s.Workspaces...)
	seen := make(map[*LayoutNode]*LayoutNode)
	var cloneNode func(*LayoutNode) *LayoutNode
	cloneNode = func(node *LayoutNode) *LayoutNode {
		if node == nil {
			return nil
		}
		if previous := seen[node]; previous != nil {
			return previous
		}
		copyNode := *node
		seen[node] = &copyNode
		copyNode.First = cloneNode(node.First)
		copyNode.Second = cloneNode(node.Second)
		return &copyNode
	}
	for i := range cloned.Workspaces {
		cloned.Workspaces[i].Layout = cloneNode(s.Workspaces[i].Layout)
	}
	return cloned
}

func (s WorkspaceState) Validate() error {
	if s.Version != WorkspaceStateVersion {
		return fmt.Errorf("unsupported workspace state version %d", s.Version)
	}
	if len(s.Workspaces) > maxWorkspaceNodes {
		return errors.New("too many workspaces")
	}
	ids := make(map[string]bool, len(s.Workspaces))
	sessions := make(map[string]string)
	seen := make(map[*LayoutNode]bool)
	var validateNode func(*LayoutNode, string, int) error
	validateNode = func(node *LayoutNode, workspaceID string, depth int) error {
		if depth > maxWorkspaceDepth {
			return errors.New("workspace layout exceeds maximum depth")
		}
		if seen[node] {
			return errors.New("workspace layout contains a repeated node")
		}
		seen[node] = true
		if len(seen) > maxWorkspaceNodes {
			return errors.New("workspace layouts exceed maximum node count")
		}
		if node.First == nil && node.Second == nil {
			if strings.TrimSpace(node.SessionID) == "" || node.Axis != "" || node.Ratio != 0 {
				return errors.New("invalid workspace session leaf")
			}
			if _, exists := sessions[node.SessionID]; exists {
				return fmt.Errorf("duplicate workspace session %q", node.SessionID)
			}
			sessions[node.SessionID] = workspaceID
			return nil
		}
		if node.First == nil || node.Second == nil || node.SessionID != "" {
			return errors.New("workspace split must have two children and no session")
		}
		if node.Axis != SplitColumns && node.Axis != SplitRows {
			return fmt.Errorf("invalid workspace split axis %q", node.Axis)
		}
		if math.IsNaN(node.Ratio) || math.IsInf(node.Ratio, 0) || node.Ratio <= 0 || node.Ratio >= 1 {
			return errors.New("workspace split ratio must be between zero and one")
		}
		if err := validateNode(node.First, workspaceID, depth+1); err != nil {
			return err
		}
		return validateNode(node.Second, workspaceID, depth+1)
	}
	for _, workspace := range s.Workspaces {
		if strings.TrimSpace(workspace.ID) == "" || strings.TrimSpace(workspace.Name) == "" {
			return errors.New("workspace ID and name must not be empty")
		}
		if ids[workspace.ID] {
			return fmt.Errorf("duplicate workspace ID %q", workspace.ID)
		}
		ids[workspace.ID] = true
		if workspace.Sidebar.PreferredWidth < 0 {
			return errors.New("workspace sidebar width must not be negative")
		}
		if workspace.Layout != nil {
			if err := validateNode(workspace.Layout, workspace.ID, 1); err != nil {
				return fmt.Errorf("workspace %q: %w", workspace.ID, err)
			}
		}
		if workspace.FocusedSessionID != "" && sessions[workspace.FocusedSessionID] != workspace.ID {
			return fmt.Errorf("workspace %q focus is not in its layout", workspace.ID)
		}
	}
	if len(s.Workspaces) == 0 {
		if s.ActiveID != "" {
			return errors.New("empty workspace state has an active workspace")
		}
	} else if !ids[s.ActiveID] {
		return errors.New("active workspace is not in workspace state")
	}
	return nil
}

func decodeWorkspaceState(data []byte) (WorkspaceState, error) {
	var state WorkspaceState
	if len(data) > maxWorkspaceBytes {
		return state, errors.New("workspace state exceeds maximum size")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&state); err != nil {
		return WorkspaceState{}, fmt.Errorf("decoding workspace state: %w", err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return WorkspaceState{}, errors.New("unexpected trailing workspace state data")
	}
	if err := state.Validate(); err != nil {
		return WorkspaceState{}, err
	}
	return state, nil
}

func (s *Store) ensureWorkspaces(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS workspace_state (
			id INTEGER PRIMARY KEY CHECK (id = 1),
			state BLOB NOT NULL
		)
	`)
	if err != nil {
		return fmt.Errorf("initializing workspace state: %w", err)
	}
	return nil
}

// GetWorkspaces returns a detached snapshot. sql.ErrNoRows means no arrangement
// has been saved; invalid or unsupported state must not be treated as empty.
func (s *Store) GetWorkspaces(ctx context.Context) (WorkspaceState, error) {
	if err := s.ensureWorkspaces(ctx); err != nil {
		return WorkspaceState{}, err
	}
	var data []byte
	if err := s.db.QueryRowContext(ctx, `SELECT state FROM workspace_state WHERE id = 1`).Scan(&data); err != nil {
		return WorkspaceState{}, fmt.Errorf("reading workspace state: %w", err)
	}
	return decodeWorkspaceState(data)
}

// SaveWorkspaces atomically replaces an arrangement, refusing to overwrite
// corrupt state or a schema version this binary does not understand.
func (s *Store) SaveWorkspaces(ctx context.Context, state WorkspaceState) error {
	if err := state.Validate(); err != nil {
		return err
	}
	data, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("encoding workspace state: %w", err)
	}
	if len(data) > maxWorkspaceBytes {
		return errors.New("workspace state exceeds maximum size")
	}
	if err := s.ensureWorkspaces(ctx); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("starting workspace state transaction: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // rollback after commit is harmless
	var previous []byte
	err = tx.QueryRowContext(ctx, `SELECT state FROM workspace_state WHERE id = 1`).Scan(&previous)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("reading previous workspace state: %w", err)
	}
	if err == nil {
		if _, err := decodeWorkspaceState(previous); err != nil {
			return fmt.Errorf("preserving unreadable workspace state: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO workspace_state (id, state) VALUES (1, ?)
		ON CONFLICT(id) DO UPDATE SET state = excluded.state
	`, data); err != nil {
		return fmt.Errorf("saving workspace state: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing workspace state: %w", err)
	}
	return nil
}

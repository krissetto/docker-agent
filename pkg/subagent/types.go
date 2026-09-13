package subagent

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"time"
)

// NodeID uniquely identifies a running agent instance in a subagent tree.
type NodeID string

// idLength is the number of hex characters in a subagent node id. Five hex
// characters (20 bits) is a short, git-like sha that is easy for humans and
// models to reference; in a system with at most a few hundred concurrent
// subagents the collision probability is negligible, and callers that mint
// ids through a [Tree] retry on the rare clash anyway.
const idLength = 5

// ShortID formats an identity for display without changing its canonical target.
func ShortID(id string) string {
	runes := []rune(id)
	return string(runes[:min(idLength, len(runes))])
}

// NewID returns a fresh, git-like short-sha node id (5 hex characters).
func NewID() NodeID {
	var seed [16]byte
	_, _ = rand.Read(seed[:])
	sum := sha256.Sum256(seed[:])
	return NodeID(hex.EncodeToString(sum[:])[:idLength])
}

// SessionRootID is the synthetic tree node that represents a top-level session
// (the swarm root). Its children are the session's own subagents.
func SessionRootID(sessionID string) NodeID {
	return NodeID("root:" + sessionID)
}

// NodeState is the lifecycle of a running agent instance.
type NodeState string

const (
	NodeStarting  NodeState = "starting"
	NodeRunning   NodeState = "running"
	NodeIdle      NodeState = "idle"
	NodeCompleted NodeState = "completed"
	NodeFailed    NodeState = "failed"
	NodeStopped   NodeState = "stopped"
)

// Node represents one running agent instance in the swarm.
type Node struct {
	ID             NodeID    `json:"id"`
	Agent          string    `json:"agent"`
	Name           string    `json:"name,omitempty"`
	Description    string    `json:"description,omitempty"`
	Parent         NodeID    `json:"parent,omitempty"`
	SessionID      string    `json:"session_id,omitempty"`
	Task           string    `json:"task,omitempty"`
	State          NodeState `json:"state"`
	NeedsAttention bool      `json:"needs_attention"`
	WaitingOn      string    `json:"waiting_on,omitempty"`
	Error          string    `json:"error,omitempty"`
	// Metrics are cumulative for this node/session only; callers may sum a
	// returned subtree for aggregate totals.
	Cost         float64   `json:"cost,omitempty"`
	InputTokens  int64     `json:"input_tokens,omitempty"`
	OutputTokens int64     `json:"output_tokens,omitempty"`
	ToolCalls    int64     `json:"tool_calls,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// DisplayName returns the model-facing name for the node.
func (n Node) DisplayName() string {
	if n.Name != "" {
		return n.Name
	}
	return n.Agent
}

// NodeSnapshot is a stable, serialisable view of a node and its children.
type NodeSnapshot struct {
	Node     Node           `json:"node"`
	Children []NodeSnapshot `json:"children,omitempty"`
}

// SnapshotVersion is the current persisted topology schema.
const SnapshotVersion = 1

// Durability records the persistence guarantee used for a snapshot.
type Durability string

const (
	DurabilityVolatile Durability = "volatile"
	DurabilityDurable  Durability = "durable"
)

// Snapshot is a serialisable view of the whole running tree.
type Snapshot struct {
	Version    int            `json:"version"`
	Durability Durability     `json:"durability"`
	Root       NodeID         `json:"root,omitempty"`
	Nodes      []NodeSnapshot `json:"nodes,omitempty"`
}

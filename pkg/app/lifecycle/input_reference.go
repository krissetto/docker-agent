package lifecycle

import (
	"strings"

	"github.com/docker/docker-agent/pkg/subagent"
)

// sessionRootName is the human label for a session root. Its canonical
// "root:<sessionID>" ID stays internal for routing and is never displayed.
const sessionRootName = "root"

// InputReference is derived presentation metadata, never stored as input provenance.
type InputReference struct {
	Kind      InputReferenceKind
	ID        string
	Name      string
	Agent     string
	DisplayID string
}

type InputReferenceKind uint8

const (
	InputReferenceUnknown InputReferenceKind = iota
	InputReferenceNode
	InputReferenceParent
)

// ResolveInputReference compares canonical identities, not their textual shape.
func ResolveInputReference(snapshot *subagent.Snapshot, parentSessionID, senderID, senderName string) InputReference {
	ref := InputReference{Name: senderName, Agent: senderName, DisplayID: subagent.ShortID(senderID)}
	if isSessionRootID(senderID) {
		ref.Name, ref.DisplayID = sessionRootName, ""
	}
	if senderID == "" {
		return ref
	}
	var byID, bySession *subagent.Node
	var walk func([]subagent.NodeSnapshot)
	walk = func(nodes []subagent.NodeSnapshot) {
		for j := range nodes {
			item := &nodes[j]
			node := &item.Node
			if string(node.ID) == senderID {
				byID = node
			}
			if node.SessionID == senderID {
				bySession = node
			}
			walk(item.Children)
		}
	}
	if snapshot != nil {
		walk(snapshot.Nodes)
	}
	node := byID
	if node == nil {
		node = bySession
	}
	if node != nil {
		ref = NodeReference(string(node.ID), node.DisplayName(), node.Agent)
	}
	if parentSessionID != "" && (senderID == parentSessionID || (node != nil && node.SessionID == parentSessionID)) {
		ref.Kind, ref.ID = InputReferenceParent, parentSessionID
	}
	return ref
}

// NodeReference is the display identity of a tree node. The session root shows
// as a bare "root"; every other node keeps its short ID for disambiguation.
func NodeReference(id, name, agent string) InputReference {
	ref := InputReference{Kind: InputReferenceNode, ID: id, Name: name, Agent: agent, DisplayID: subagent.ShortID(id)}
	if isSessionRootID(id) {
		ref.Name, ref.DisplayID = sessionRootName, ""
	}
	return ref
}

func isSessionRootID(id string) bool { return strings.HasPrefix(id, "root:") }

func (r InputReference) Label() string {
	id := r.DisplayID
	if r.Name == "" {
		return id
	}
	if id == "" {
		return r.Name
	}
	return r.Name + " (" + id + ")"
}

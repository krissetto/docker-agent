package lifecycle

import (
	"github.com/docker/docker-agent/pkg/subagent"
)

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
		ref.Kind, ref.ID = InputReferenceNode, string(node.ID)
		ref.Name, ref.Agent, ref.DisplayID = node.DisplayName(), node.Agent, subagent.ShortID(string(node.ID))
	}
	if parentSessionID != "" && (senderID == parentSessionID || (node != nil && node.SessionID == parentSessionID)) {
		ref.Kind, ref.ID = InputReferenceParent, parentSessionID
	}
	return ref
}

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

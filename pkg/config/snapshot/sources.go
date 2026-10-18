package snapshot

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
)

// SourceDescriptor is a non-secret provenance projection of one frozen node.
type SourceDescriptor struct {
	Key           string
	Name          string
	ParentDir     string
	References    []string
	ContentDigest string
}

// Sources returns deterministic defensive descriptors; frozen configuration
// contents and internal maps are never exposed.
func (m *Manifest) Sources() []SourceDescriptor {
	keys := make([]string, 0, len(m.doc.Sources))
	for key := range m.doc.Sources {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make([]SourceDescriptor, 0, len(keys))
	for _, key := range keys {
		node := m.doc.Sources[key]
		sum := sha256.Sum256([]byte(node.YAML))
		out = append(out, SourceDescriptor{Key: key, Name: node.Name, ParentDir: node.ParentDir, References: append([]string(nil), node.References...), ContentDigest: hex.EncodeToString(sum[:])})
	}
	return out
}

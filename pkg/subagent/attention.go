package subagent

import (
	"encoding/json"
	"slices"
)

// AttentionIdentity encodes namespaced occurrence IDs without making Node
// noncomparable. Ordering and duplicate inputs do not affect the identity.
func AttentionIdentity(ids ...string) string {
	ids = slices.Clone(ids)
	slices.Sort(ids)
	ids = slices.Compact(ids)
	ids = slices.DeleteFunc(ids, func(id string) bool { return id == "" })
	if len(ids) == 0 {
		return ""
	}
	encoded, _ := json.Marshal(ids)
	return string(encoded)
}

// AttentionOccurrences decodes the opaque identity. Old or malformed snapshots
// have no usable occurrences and retain the consumer's legacy transition fallback.
func AttentionOccurrences(identity string) []string {
	var ids []string
	if json.Unmarshal([]byte(identity), &ids) != nil {
		return nil
	}
	return slices.DeleteFunc(ids, func(id string) bool { return id == "" })
}

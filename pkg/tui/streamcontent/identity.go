// Package streamcontent tracks logical assistant-message boundaries.
package streamcontent

import (
	"strconv"
	"strings"
)

type Identity struct {
	SessionID string
	MessageID string
}

// Tracker gives legacy events without message IDs a per-message identity.
type Tracker struct {
	sequence uint64
	legacy   map[string]string
	media    map[string]uint64
}

func (t *Tracker) Resolve(sessionID, messageID string) Identity {
	if messageID == "" {
		if t.legacy == nil {
			t.legacy = make(map[string]string)
		}
		messageID = t.legacy[sessionID]
		if messageID == "" {
			t.sequence++
			messageID = "legacy:" + strconv.FormatUint(t.sequence, 10)
			t.legacy[sessionID] = messageID
		}
	}
	return Identity{SessionID: sessionID, MessageID: messageID}
}

func (t *Tracker) Finish(sessionID string) { delete(t.legacy, sessionID) }

// IsLegacy reports whether messageID is a tracker-issued identity for an event
// that carried no runtime message ID.
func IsLegacy(messageID string) bool { return strings.HasPrefix(messageID, "legacy:") }

// MediaID reports the placeholder ID already issued for a generated-media key.
func (t *Tracker) MediaID(key string) (uint64, bool) {
	id, ok := t.media[key]
	return id, ok
}

// RememberMedia records the placeholder ID issued for a generated-media key.
// Media keys outlive streams: a replayed reference keeps its item for the page.
func (t *Tracker) RememberMedia(key string, id uint64) {
	if t.media == nil {
		t.media = make(map[string]uint64)
	}
	t.media[key] = id
}

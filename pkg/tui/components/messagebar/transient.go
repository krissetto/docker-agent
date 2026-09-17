package messagebar

import (
	"fmt"
	"strings"
	"time"
)

const (
	DefaultLifetime    = 3 * time.Second
	MaxAggregateOwners = 8
	MaxBurstIDs        = 256
)

// EventKind names canonical facts, not stream or observer lifecycle signals.
type EventKind uint8

const (
	CompletedTurn EventKind = iota + 1
	SpawnedSubagent
)

// Event is supplied by the canonical adapter. Sequence must increase strictly
// within (Owner, Kind); ID is the durable turn or spawned-session identity.
// Replay includes hydration and reattachment. Adapters must suppress those
// sources and retain their canonical high-water marks across owner eviction.
type Event struct {
	Owner    string
	ID       string
	Kind     EventKind
	Sequence uint64
	Replay   bool
}

// Snapshot is clock-free presentation data, suitable for a pinned lean footer.
// Its generation is an aggregate revision, not a component ownership Token.
type Snapshot struct {
	Owner            string
	Generation       uint64
	Text             string
	Deadline         time.Time
	CompletedTurns   int
	SpawnedSubagents int
}

type eventIdentity struct {
	kind EventKind
	id   string
}

type burst struct {
	snapshot  Snapshot
	seen      map[eventIdentity]struct{}
	highWater [2]uint64
	touched   uint64
}

// Aggregator retains only bounded, ephemeral owner bursts, never notification
// history. Its zero value is ready for use. Owners are session identities, not
// focus positions: switching focus does not reset counts or extend deadlines.
// Inactive owners are evicted least-recently-updated. The capped ID set provides
// extra within-burst duplicate protection; after it fills, canonical sequence
// and the adapter's business-once guarantee keep counting without truncation.
// Calls belong to the consuming event loop; there are no timers or goroutines.
type Aggregator struct {
	owners     map[string]*burst
	generation uint64
}

// Add coalesces an exactly-once canonical fact into its owner's current burst.
// Rejected facts never extend the deadline or create a presentation revision.
func (a *Aggregator) Add(event Event, now time.Time) (Snapshot, bool) {
	if event.Replay || event.Owner == "" || event.ID == "" || event.Sequence == 0 ||
		(event.Kind != CompletedTurn && event.Kind != SpawnedSubagent) {
		return Snapshot{}, false
	}
	if a.owners == nil {
		a.owners = make(map[string]*burst)
	}
	b := a.owners[event.Owner]
	if b == nil {
		if len(a.owners) == MaxAggregateOwners {
			var oldest string
			var touched uint64
			for owner, candidate := range a.owners {
				if oldest == "" || candidate.touched < touched {
					oldest, touched = owner, candidate.touched
				}
			}
			delete(a.owners, oldest)
		}
		b = &burst{}
		a.owners[event.Owner] = b
	}
	index := int(event.Kind - 1)
	if event.Sequence <= b.highWater[index] {
		return Snapshot{}, false
	}
	b.highWater[index] = event.Sequence
	if !now.Before(b.snapshot.Deadline) {
		b.snapshot = Snapshot{Owner: event.Owner}
		b.seen = make(map[eventIdentity]struct{})
	}
	identity := eventIdentity{kind: event.Kind, id: event.ID}
	if _, exists := b.seen[identity]; exists {
		return Snapshot{}, false
	}
	if len(b.seen) < MaxBurstIDs {
		b.seen[identity] = struct{}{}
	}
	switch event.Kind {
	case CompletedTurn:
		b.snapshot.CompletedTurns++
	case SpawnedSubagent:
		b.snapshot.SpawnedSubagents++
	}
	a.generation++
	b.touched = a.generation
	b.snapshot.Generation = a.generation
	deadline := now.Add(DefaultLifetime)
	if deadline.After(b.snapshot.Deadline) {
		b.snapshot.Deadline = deadline
	}
	var parts []string
	if count := b.snapshot.CompletedTurns; count > 0 {
		label := "turn"
		if count != 1 {
			label = "turns"
		}
		parts = append(parts, fmt.Sprintf("%d %s completed", count, label))
	}
	if count := b.snapshot.SpawnedSubagents; count > 0 {
		label := "subagent"
		if count != 1 {
			label = "subagents"
		}
		parts = append(parts, fmt.Sprintf("%d %s spawned", count, label))
	}
	b.snapshot.Text = strings.Join(parts, " · ")
	return b.snapshot, true
}

// Current reads without touching recency, deadlines, identity, or generation.
func (a *Aggregator) Current(owner string, now time.Time) (Snapshot, bool) {
	b := a.owners[owner]
	if b == nil || b.snapshot.Text == "" || !now.Before(b.snapshot.Deadline) {
		return Snapshot{}, false
	}
	return b.snapshot, true
}

// Category expresses notice intent independently of Severity's color enum.
type Category uint8

const (
	Notice Category = iota
	Background
	Hint
	Cancellation
)

// CanReplace is shared by full-screen and lean presentation owners. Cancellation
// outranks errors, errors outrank warnings, and all three protect against hints
// and background summaries. Owner identity never grants a priority bypass.
func CanReplace(current, incoming Message) bool {
	if current.empty() {
		return true
	}
	return noticePriority(incoming) >= noticePriority(current)
}

func noticePriority(message Message) int {
	if message.Category == Cancellation {
		return 5
	}
	switch message.Severity {
	case Error:
		return 4
	case Warning:
		return 3
	}
	switch message.Category {
	case Background:
		return 0
	case Hint:
		return 1
	default:
		return 2
	}
}

// Token identifies a single accepted component update. The presentation owner
// schedules one deadline command and passes this token back to Expire; neither
// the aggregator nor the component owns a hold timer or an animation lease.
type Token struct {
	Owner      string
	Generation uint64
}

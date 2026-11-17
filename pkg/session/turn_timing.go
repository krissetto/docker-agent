package session

// TurnTimingVersion is the schema version of TurnTiming. Readers must treat any
// other version as absent.
const TurnTimingVersion = 1

// TurnTimingBasisActiveWall names what DurationMs measures: monotonic wall time
// from root turn start to settlement, minus time blocked on human interactions.
// Parallel tools and subagents count once as elapsed wall time, never summed.
const TurnTimingBasisActiveWall = "active_wall_excluding_human_wait"

// Turn timing outcomes mirror runtime.TurnOutcome values.
const (
	TurnTimingOutcomeCompleted = "completed"
	TurnTimingOutcomeCanceled  = "canceled"
	TurnTimingOutcomeFailed    = "failed"
)

// TurnTiming is the durable timing of one settled root (human-initiated) turn,
// keyed by the turn ID. Absent timing means unknown; it is never inferred.
type TurnTiming struct {
	Version    int    `json:"version"`
	Basis      string `json:"basis"`
	TurnID     string `json:"turn_id"`
	DurationMs int64  `json:"duration_ms"`
	WaitingMs  int64  `json:"waiting_ms,omitempty"`
	Outcome    string `json:"outcome"`
}

// valid reports whether t is a complete timing this version understands.
func (t TurnTiming) valid() bool {
	switch t.Outcome {
	case TurnTimingOutcomeCompleted, TurnTimingOutcomeCanceled, TurnTimingOutcomeFailed:
	default:
		return false
	}
	return t.Version == TurnTimingVersion && t.Basis == TurnTimingBasisActiveWall && t.TurnID != "" &&
		t.DurationMs >= 0 && t.WaitingMs >= 0
}

// TurnTiming returns the persisted timing for turnID. It returns false for
// unknown, legacy (never measured), or incomplete records.
func (s *Session) TurnTiming(turnID string) (TurnTiming, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	timing, ok := s.TurnTimings[turnID]
	if !ok || timing.TurnID != turnID || !timing.valid() {
		return TurnTiming{}, false
	}
	return timing, true
}

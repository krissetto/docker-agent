package runtime

import (
	"testing"

	"github.com/docker/docker-agent/pkg/session"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSessionDriverAdmissionParity(t *testing.T) {
	type state func(*sessionDriver)
	tests := []struct {
		name      string
		state     state
		op        SessionOperation
		kind      SessionErrorKind
		operation SessionOperation
		reason    SessionErrorReason
		limit     int
	}{
		{name: "stopped post", state: func(d *sessionDriver) { d.stopped = true }, op: SessionOperationPost, kind: SessionErrorStopped, operation: SessionOperationPost},
		{name: "stopped steer", state: func(d *sessionDriver) { d.stopped = true }, op: SessionOperationSteer, kind: SessionErrorStopped, operation: SessionOperationSteer},
		{name: "stopped compact", state: func(d *sessionDriver) { d.stopped = true }, op: SessionOperationCompact, kind: SessionErrorStopped, operation: SessionOperationCompact},
		{name: "stopped pause", state: func(d *sessionDriver) { d.stopped = true }, op: SessionOperationPause, kind: SessionErrorStopped, operation: SessionOperationPause},
		{name: "stopped skill", state: func(d *sessionDriver) { d.stopped = true }, op: SessionOperationRunSkill, kind: SessionErrorStopped, operation: SessionOperationRunSkill, reason: SessionErrorReasonBusy},
		{name: "stopped switch", state: func(d *sessionDriver) { d.stopped = true }, op: SessionOperationSwitchAgent, kind: SessionErrorStopped, operation: SessionOperationSwitchAgent, reason: SessionErrorReasonBusy},
		{name: "running allows compact", state: func(d *sessionDriver) { d.running = true }, op: SessionOperationCompact},
		{name: "running rejects skill", state: func(d *sessionDriver) { d.running = true }, op: SessionOperationRunSkill, kind: SessionErrorCapacity, operation: SessionOperationRunSkill, reason: SessionErrorReasonBusy},
		{name: "running rejects switch", state: func(d *sessionDriver) { d.running = true }, op: SessionOperationSwitchAgent, kind: SessionErrorCapacity, operation: SessionOperationSwitchAgent, reason: SessionErrorReasonBusy},
		{name: "starting allows post wait", state: func(d *sessionDriver) { d.starting = true }, op: SessionOperationPost},
		{name: "starting rejects compact", state: func(d *sessionDriver) { d.starting = true }, op: SessionOperationCompact, kind: SessionErrorCapacity, operation: SessionOperationCompactBusy, reason: SessionErrorReasonBusy, limit: defaultMaxSubagentMailbox},
		{name: "wake rejects skill", state: func(d *sessionDriver) { d.wakeRunning = true }, op: SessionOperationRunSkill, kind: SessionErrorCapacity, operation: SessionOperationRunSkill, reason: SessionErrorReasonBusy},
		{name: "cancelling rejects switch", state: func(d *sessionDriver) { d.cancelling = true }, op: SessionOperationSwitchAgent, kind: SessionErrorCapacity, operation: SessionOperationSwitchAgent, reason: SessionErrorReasonBusy},
		{name: "reclaiming rejects skill", state: func(d *sessionDriver) { d.reclaiming = true }, op: SessionOperationRunSkill, kind: SessionErrorCapacity, operation: SessionOperationRunSkill, reason: SessionErrorReasonBusy},
		{name: "compaction accepts post", state: func(d *sessionDriver) { d.compactReserved = true }, op: SessionOperationPost},
		{name: "compaction accepts steer demotion", state: func(d *sessionDriver) { d.compactReserved = true }, op: SessionOperationSteer},
		{name: "compaction rejects compact", state: func(d *sessionDriver) { d.compactReserved = true }, op: SessionOperationCompact, kind: SessionErrorCapacity, operation: SessionOperationCompactBusy, reason: SessionErrorReasonBusy, limit: defaultMaxSubagentMailbox},
		{name: "pause rejects skill", state: func(d *sessionDriver) { d.pauseCh = make(chan struct{}) }, op: SessionOperationRunSkill, kind: SessionErrorCapacity, operation: SessionOperationRunSkill, reason: SessionErrorReasonBusy},
		{name: "pending rejects compact distinctly", state: func(d *sessionDriver) { d.pending = []QueuedMessage{{}} }, op: SessionOperationCompact, kind: SessionErrorCapacity, operation: SessionOperationCompactPending, reason: SessionErrorReasonPending, limit: defaultMaxSubagentMailbox},
		{name: "pending rejects switch", state: func(d *sessionDriver) { d.pending = []QueuedMessage{{}} }, op: SessionOperationSwitchAgent, kind: SessionErrorCapacity, operation: SessionOperationSwitchAgent, reason: SessionErrorReasonBusy},
		{name: "steering rejects compact distinctly", state: func(d *sessionDriver) { d.steering = []QueuedMessage{{}} }, op: SessionOperationCompact, kind: SessionErrorCapacity, operation: SessionOperationCompactPending, reason: SessionErrorReasonPending, limit: defaultMaxSubagentMailbox},
		{name: "interaction allows skill", state: func(d *sessionDriver) { d.interactions["i"] = sessionInteraction{} }, op: SessionOperationRunSkill},
		{name: "interaction rejects switch", state: func(d *sessionDriver) { d.interactions["i"] = sessionInteraction{} }, op: SessionOperationSwitchAgent, kind: SessionErrorCapacity, operation: SessionOperationSwitchAgent, reason: SessionErrorReasonBusy},
		{name: "skill rejects post as skill busy", state: func(d *sessionDriver) { d.skillOperationID = "skill" }, op: SessionOperationPost, kind: SessionErrorCapacity, operation: SessionOperationSkillBusy, reason: SessionErrorReasonBusy},
		{name: "skill rejects pause as skill busy", state: func(d *sessionDriver) { d.skillOperationID = "skill" }, op: SessionOperationPause, kind: SessionErrorCapacity, operation: SessionOperationSkillBusy, reason: SessionErrorReasonBusy},
		{name: "skill rejects compact as compact busy", state: func(d *sessionDriver) { d.skillOperationID = "skill" }, op: SessionOperationCompact, kind: SessionErrorCapacity, operation: SessionOperationCompactBusy, reason: SessionErrorReasonBusy, limit: defaultMaxSubagentMailbox},
		{name: "switch reservation rejects post as switch agent", state: func(d *sessionDriver) { d.switchReserved = true }, op: SessionOperationPost, kind: SessionErrorCapacity, operation: SessionOperationSwitchAgent, reason: SessionErrorReasonBusy},
		{name: "switch reservation rejects skill", state: func(d *sessionDriver) { d.switchReserved = true }, op: SessionOperationRunSkill, kind: SessionErrorCapacity, operation: SessionOperationRunSkill, reason: SessionErrorReasonBusy},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := newSessionDriver(&LocalRuntime{maxPendingMailbox: defaultMaxSubagentMailbox}, &session.Session{ID: "session-id"})
			tt.state(d)
			d.mu.Lock()
			err := d.admitLocked(tt.op)
			d.mu.Unlock()
			if tt.kind == "" {
				assert.Nil(t, err)
				return
			}
			require.Error(t, err)
			assert.Equal(t, tt.kind, err.Kind)
			assert.Equal(t, tt.operation, err.Operation)
			assert.Equal(t, tt.reason, err.Reason)
			assert.Equal(t, tt.limit, err.Limit)
			assert.Equal(t, "session-id", err.SessionID)
		})
	}
}

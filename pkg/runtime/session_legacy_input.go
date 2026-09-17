package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/model/provider"
	"github.com/docker/docker-agent/pkg/session"
)

// SessionLegacyInput translates old HTTP input shapes into the canonical
// driver's transcript, mailbox and generation. It owns no alternate queue.
type SessionLegacyInput interface {
	RunLegacyTurn(ctx context.Context, inputs []TurnInput, requestID string) (Submission, error)
	QueueLegacyFollowUps(ctx context.Context, inputs []TurnInput, requestID string) (LegacyFollowUpResult, error)
	LegacyQueueStatus(ctx context.Context) (LegacyQueueStatus, error)
}
type SessionLegacyModelInput interface {
	RunLegacyTurnWithModel(ctx context.Context, inputs []TurnInput, modelRef, requestID string) (Submission, error)
}

type LegacyCommandMessage struct {
	Role  chat.MessageRole
	Input TurnInput
}
type SessionLegacyCommandInput interface {
	RunLegacyCommandTurn(ctx context.Context, messages []LegacyCommandMessage, modelRef, requestID string) (Submission, error)
}

type (
	LegacyFollowUpResult struct{ Streaming, Duplicate bool }
	LegacyQueueStatus    struct{ SteerDepth, SteerCapacity, FollowUpDepth, FollowUpCapacity int }
)

func (h *sessionHandle) LegacyQueueStatus(ctx context.Context) (LegacyQueueStatus, error) {
	if err := ctx.Err(); err != nil {
		return LegacyQueueStatus{}, err
	}
	d := h.driver
	d.mu.Lock()
	defer d.mu.Unlock()
	return LegacyQueueStatus{SteerDepth: len(d.steering), SteerCapacity: d.pendingLimit(), FollowUpDepth: len(d.pending), FollowUpCapacity: d.pendingLimit()}, nil
}

func (h *sessionHandle) RunLegacyTurn(ctx context.Context, inputs []TurnInput, requestID string) (Submission, error) {
	return h.RunLegacyTurnWithModel(ctx, inputs, "", requestID)
}

func (h *sessionHandle) RunLegacyTurnWithModel(ctx context.Context, inputs []TurnInput, modelRef, requestID string) (Submission, error) {
	return h.runLegacyTurn(ctx, inputs, nil, modelRef, requestID)
}

func (h *sessionHandle) RunLegacyCommandTurn(ctx context.Context, messages []LegacyCommandMessage, modelRef, requestID string) (Submission, error) {
	inputs := make([]TurnInput, len(messages))
	for i, message := range messages {
		inputs[i] = message.Input
	}
	return h.runLegacyTurn(ctx, inputs, messages, modelRef, requestID)
}

func (h *sessionHandle) runLegacyTurn(ctx context.Context, inputs []TurnInput, commands []LegacyCommandMessage, modelRef, requestID string) (Submission, error) {
	var providers []provider.Provider
	id, err := sessionInputID(h.sessionID, requestID)
	if err != nil {
		return Submission{}, err
	}
	d := h.driver
	// The same generation-admission lock used by v2 prevents append/start races.
	d.r.sessionDrivers.runMu.Lock()
	d.mu.Lock()
	if err := ctx.Err(); err != nil {
		d.mu.Unlock()
		d.r.sessionDrivers.runMu.Unlock()
		return Submission{}, err
	}
	if err := d.admitLocked(SessionOperationPost); err != nil {
		d.mu.Unlock()
		d.r.sessionDrivers.runMu.Unlock()
		return Submission{}, err
	}
	marker := session.UserMessage("")
	marker.Message.CreatedAt = ""
	marker.Implicit = true
	marker.Pending = true
	marker.Accepted = true
	marker.TurnID = id
	marker.InputMode = "legacy_run"
	marker.InputOrigin = session.InputOriginUser
	marker.Message.Model = modelRef
	var requestData []byte
	if commands != nil {
		requestData, _ = json.Marshal(commands)
	} else {
		requestData, _ = json.Marshal(inputs)
	}
	marker.Message.ReasoningContent = string(requestData)
	receipt, found, lookupErr := d.lookupLegacyBatchLocked(ctx, "run:"+id, []session.Item{session.NewMessageItem(marker)})
	if lookupErr != nil {
		d.mu.Unlock()
		d.r.sessionDrivers.runMu.Unlock()
		return Submission{}, lookupErr
	}
	if found {
		err = d.reconcileLegacyBatchLocked(ctx, receipt)
		d.mu.Unlock()
		d.r.sessionDrivers.runMu.Unlock()
		if err == nil {
			d.WakePending()
		}
		return Submission{SessionID: h.sessionID, TurnID: id}, err
	}
	if d.running() || d.starting() || d.settling() || d.compactReserved || d.skillOperationID != "" {
		d.mu.Unlock()
		d.r.sessionDrivers.runMu.Unlock()
		return Submission{}, &SessionError{Kind: SessionErrorConflict, SessionID: h.sessionID, Operation: "legacy_run", Reason: SessionErrorReasonBusy}
	}
	originalAgent := d.sess.AgentName
	activeAgent := originalAgent
	// Validate all command targets against the pre-batch active agent before any
	// mutation. Resolution also uses that agent even when several switches occur.
	targets := make(map[int]string)
	for i, message := range commands {
		if message.Role != chat.MessageRoleUser {
			continue
		}
		cmd, _, ok := LookupCommand(ctx, h.runtime, originalAgent, message.Input.Content)
		if !ok || cmd.Agent == "" {
			continue
		}
		if _, targetErr := h.runtime.team.Agent(cmd.Agent); targetErr != nil {
			d.mu.Unlock()
			d.r.sessionDrivers.runMu.Unlock()
			return Submission{}, targetErr
		}
		targets[i] = cmd.Agent
		activeAgent = cmd.Agent
	}
	if modelRef != "" {
		if !h.runtime.SupportsModelSwitching() {
			d.mu.Unlock()
			d.r.sessionDrivers.runMu.Unlock()
			return Submission{}, sessionUnsupported(h.sessionID, SessionOperationSetModel)
		}
		providers, err = h.runtime.resolveModelProviders(ctx, originalAgent, modelRef)
		if err != nil {
			d.mu.Unlock()
			d.r.sessionDrivers.runMu.Unlock()
			return Submission{}, err
		}
	}
	activeRef := modelRef
	if activeAgent != originalAgent {
		candidate := d.sess.OwnSnapshot()
		candidate.AgentName = activeAgent
		activeRef, providers, err = h.runtime.resolveSessionModelBinding(ctx, candidate, "")
		if err != nil {
			d.mu.Unlock()
			d.r.sessionDrivers.runMu.Unlock()
			return Submission{}, err
		}
	}
	// Command evaluators may call tools; release d.mu while the existing
	// maintenance reservation rejects concurrent input/agent mutation.
	if len(targets) != 0 {
		d.switchReserved = true
		d.mu.Unlock()
		inputs = slices.Clone(inputs)
		for i := range inputs {
			if _, ok := targets[i]; ok {
				inputs[i].Content = ResolveCommand(ctx, h.runtime, originalAgent, inputs[i].Content)
			}
		}
		d.mu.Lock()
		d.switchReserved = false
		if d.stopped {
			d.mu.Unlock()
			d.r.sessionDrivers.runMu.Unlock()
			return Submission{}, ErrSessionStopped
		}
	}
	items := make([]session.Item, 0, len(inputs)+1)
	for _, input := range inputs {
		if input.Retry {
			d.mu.Unlock()
			d.r.sessionDrivers.runMu.Unlock()
			return Submission{}, errors.New("legacy batch input cannot request retry")
		}
		message := session.UserMessage(input.Content, input.MultiContent...)
		message.Message.CreatedAt = ""
		message.InputOrigin = session.InputOriginUser
		items = append(items, session.NewMessageItem(message))
	}
	marker.AgentName = activeAgent
	items = append(items, session.NewMessageItem(marker))
	if !limitAllows(len(d.pending), d.pendingLimit()) {
		d.mu.Unlock()
		d.r.sessionDrivers.runMu.Unlock()
		return Submission{}, ErrSessionCapacity
	}
	var duplicate bool
	if modelRef == "" && activeAgent == originalAgent {
		duplicate, err = d.appendLegacyBatchLocked(ctx, "run:"+id, items)
	} else {
		modelStore, ok := d.r.sessionStore.(session.ItemBatchBindingAppender)
		if !ok {
			err = UnsupportedSessionOperation(h.sessionID, "atomic_model_input_batch")
		} else {
			receipt, appendErr := modelStore.AppendItemsWithBinding(ctx, h.sessionID, "run:"+id, items, originalAgent, modelRef, activeAgent)
			err = appendErr
			duplicate = receipt.Duplicate
			if err == nil && !duplicate {
				for i, item := range items {
					item.Message.ID = receipt.IDs[i]
					d.sess.AddMessageAt(item.Message)
				}
				if modelRef != "" {
					d.sess.SetAgentModelOverride(originalAgent, modelRef)
				}
				d.sess.AgentName = activeAgent
				d.modelRef = activeRef
				d.modelProviders = slices.Clone(providers)
				d.bindingVersion++
			}
		}
	}
	if err == nil && !duplicate {
		message := queuedSessionInput(marker, len(d.sess.MessagesSnapshot())-1, d.r.sessionStore != nil)
		d.pending = append([]QueuedMessage{message}, d.pending...)
		d.authorizeViewLocked()
	}
	d.mu.Unlock()
	var runCtx context.Context
	var generation uint64
	var callbacks []func()
	var startErr error
	if err == nil && !duplicate {
		runCtx, generation, callbacks, startErr = d.prepareStartAdmissionLocked(d.r.lifetime(), true)
	}
	d.r.sessionDrivers.runMu.Unlock()
	if err != nil {
		return Submission{}, err
	}
	if !duplicate {
		if startErr != nil {
			d.schedulePendingRetry()
		} else {
			for _, fn := range callbacks {
				fn()
			}
			d.startWake(runCtx, generation)
		}
	}
	return Submission{SessionID: h.sessionID, TurnID: id}, nil
}

func (h *sessionHandle) QueueLegacyFollowUps(ctx context.Context, inputs []TurnInput, requestID string) (LegacyFollowUpResult, error) {
	id, err := sessionInputID(h.sessionID, requestID)
	if err != nil {
		return LegacyFollowUpResult{}, err
	}
	d := h.driver
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return LegacyFollowUpResult{}, err
	}
	if err := d.admitLocked(SessionOperationPost); err != nil {
		return LegacyFollowUpResult{}, err
	}
	streaming := d.running() || d.starting() || d.settling()
	items := make([]session.Item, 0, len(inputs))
	for i, input := range inputs {
		if input.Retry {
			return LegacyFollowUpResult{}, errors.New("legacy followup cannot request retry")
		}
		msg := session.UserMessage(input.Content, input.MultiContent...)
		msg.Message.CreatedAt = ""
		msg.Pending = true
		msg.Accepted = true
		msg.TurnID = id + ":" + strconv.Itoa(i)
		msg.InputOrigin = session.InputOriginUser
		msg.InputMode = "legacy_followup"
		items = append(items, session.NewMessageItem(msg))
	}
	receipt, found, err := d.lookupLegacyBatchLocked(ctx, "followup:"+id, items)
	if err != nil {
		return LegacyFollowUpResult{}, err
	}
	if found {
		err = d.reconcileLegacyBatchLocked(ctx, receipt)
		return LegacyFollowUpResult{Streaming: streaming, Duplicate: true}, err
	}
	if d.pendingLimit() >= 0 && len(inputs) > d.pendingLimit()-len(d.pending) {
		return LegacyFollowUpResult{}, ErrSessionCapacity
	}
	duplicate, err := d.appendLegacyBatchLocked(ctx, "followup:"+id, items)
	if err != nil {
		return LegacyFollowUpResult{}, err
	}
	if !duplicate {
		position := len(d.sess.MessagesSnapshot()) - len(items)
		for i, item := range items {
			msg := queuedSessionInput(item.Message, position+i, d.r.sessionStore != nil)
			d.pending = append(d.pending, msg)
			d.events.PublishForRequest(h.sessionID, msg.RequestID, inputEventMetadata(PendingUserMessageAccepted(h.sessionID, msg.RequestID, msg.Content, msg.MultiContent, position+i), msg))
		}
	}
	return LegacyFollowUpResult{Streaming: streaming, Duplicate: duplicate}, nil
}

func (d *sessionDriver) appendLegacyBatchLocked(ctx context.Context, requestID string, items []session.Item) (bool, error) {
	store, ok := d.r.sessionStore.(session.ItemBatchAppender)
	if !ok {
		return false, UnsupportedSessionOperation(d.sessionIDLocked(), "atomic_input_batch")
	}
	receipt, err := store.AppendItems(ctx, d.sessionIDLocked(), requestID, items)
	if err != nil {
		return false, fmt.Errorf("append input batch: %w", err)
	}
	if receipt.Duplicate {
		return true, nil
	}
	for i, item := range items {
		item.Message.ID = receipt.IDs[i]
		d.sess.AddMessageAt(item.Message)
	}
	return false, nil
}

// Legacy headless follow-ups stay dormant while idle. Explicit user submission
// or a running generation's normal successor handoff may consume the same FIFO.
func (d *sessionDriver) pendingWakeableLocked() bool {
	for _, msg := range d.pending {
		if msg.InputMode != "legacy_followup" {
			return true
		}
	}
	return false
}

func (d *sessionDriver) lookupLegacyBatchLocked(ctx context.Context, key string, items []session.Item) (session.ItemBatchReceipt, bool, error) {
	store, ok := d.r.sessionStore.(session.ItemBatchAppender)
	if !ok {
		return session.ItemBatchReceipt{}, false, UnsupportedSessionOperation(d.sessionIDLocked(), "atomic_input_batch")
	}
	return store.LookupItemBatch(ctx, d.sessionIDLocked(), key, items)
}

// Reconcile an uncertain acknowledgement from persisted rows, never the retried
// payload: a previously promoted or edited input cannot be resurrected.
func (d *sessionDriver) reconcileLegacyBatchLocked(ctx context.Context, receipt session.ItemBatchReceipt) error {
	present := map[int64]bool{}
	for _, item := range d.sess.MessagesSnapshot() {
		if item.Message != nil {
			present[item.Message.ID] = true
		}
	}
	missing := false
	for _, id := range receipt.IDs {
		if !present[id] {
			missing = true
		}
	}
	if !missing {
		return nil
	}
	stored, err := d.r.sessionStore.GetSession(ctx, d.sessionIDLocked())
	if err != nil {
		return err
	}
	if !d.running() && !d.starting() && !d.settling() {
		modelRef, providers, bindErr := d.r.resolveSessionModelBinding(ctx, stored, "")
		if bindErr != nil {
			return bindErr
		}
		d.sess.AgentName = stored.AgentName
		overrides, custom := stored.ModelStateSnapshot()
		d.sess.ReplaceModelState(overrides, custom)
		d.modelRef = modelRef
		d.modelProviders = providers
		d.bindingVersion++
	}
	receiptIDs := map[int64]bool{}
	for _, id := range receipt.IDs {
		receiptIDs[id] = true
	}
	for _, item := range stored.MessagesSnapshot() {
		msg := item.Message
		if msg == nil || present[msg.ID] || !receiptIDs[msg.ID] {
			continue
		}
		pos := d.sess.AddMessageAt(msg)
		if msg.Pending && msg.Accepted {
			queued := queuedSessionInput(msg, pos, true)
			if msg.InputMode == "legacy_run" {
				d.pending = append([]QueuedMessage{queued}, d.pending...)
			} else {
				d.pending = append(d.pending, queued)
			}
		}
	}
	return nil
}

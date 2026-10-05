package runtime

import (
	"context"
	"encoding/json"
	"errors"
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
	var result LegacyQueueStatus
	err := h.driver.ownerCall(ctx, func() error {
		d := h.driver
		result = LegacyQueueStatus{SteerDepth: len(d.steering), SteerCapacity: d.pendingLimit(), FollowUpDepth: len(d.pending), FollowUpCapacity: d.pendingLimit()}
		return nil
	})
	return result, err
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
	id, err := sessionInputID(h.sessionID, requestID)
	if err != nil {
		return Submission{}, err
	}
	d := h.driver
	marker := session.UserMessage("")
	marker.Message.CreatedAt, marker.Implicit = "", true
	marker.Pending, marker.Accepted, marker.TurnID = true, true, id
	marker.InputMode, marker.InputOrigin, marker.Message.Model = "legacy_run", session.InputOriginUser, modelRef
	var data []byte
	if commands != nil {
		data, _ = json.Marshal(commands)
	} else {
		data, _ = json.Marshal(inputs)
	}
	marker.Message.ReasoningContent = string(data)
	var originalAgent, activeAgent, activeRef string
	var providers []provider.Provider
	var items []session.Item
	_, err = d.legacyBatch(ctx, "run:"+id, []session.Item{session.NewMessageItem(marker)}, len(inputs)+1, true, func(snapshot *session.Session) error {
		originalAgent, activeAgent, activeRef = snapshot.AgentName, snapshot.AgentName, modelRef
		inputs = slices.Clone(inputs)
		for i, message := range commands {
			if message.Role != chat.MessageRoleUser {
				continue
			}
			cmd, _, ok := LookupCommand(ctx, h.runtime, originalAgent, message.Input.Content)
			if !ok || cmd.Agent == "" {
				continue
			}
			if _, err := h.runtime.team.Agent(cmd.Agent); err != nil {
				return err
			}
			activeAgent = cmd.Agent
			inputs[i].Content = ResolveCommand(ctx, h.runtime, originalAgent, inputs[i].Content)
		}
		if modelRef != "" {
			if !h.runtime.SupportsModelSwitching() {
				return sessionUnsupported(h.sessionID, SessionOperationSetModel)
			}
			var err error
			providers, err = h.runtime.resolveModelProviders(ctx, originalAgent, modelRef)
			if err != nil {
				return err
			}
		}
		if activeAgent != originalAgent {
			snapshot.AgentName = activeAgent
			var err error
			activeRef, providers, err = h.runtime.resolveSessionModelBinding(ctx, snapshot, "")
			if err != nil {
				return err
			}
		}
		for _, input := range inputs {
			if input.Retry {
				return errors.New("legacy batch input cannot request retry")
			}
			message := session.UserMessage(input.Content, input.MultiContent...)
			message.Message.CreatedAt, message.InputOrigin = "", session.InputOriginUser
			items = append(items, session.NewMessageItem(message))
		}
		marker.AgentName = activeAgent
		items = append(items, session.NewMessageItem(marker))
		return nil
	}, func(ctx context.Context, store session.ItemBatchAppender) (session.ItemBatchReceipt, error) {
		if modelRef == "" && activeAgent == originalAgent {
			return store.AppendItems(ctx, h.sessionID, "run:"+id, items)
		}
		bindingStore, ok := d.r.sessionStore.(session.ItemBatchBindingAppender)
		if !ok {
			return session.ItemBatchReceipt{}, UnsupportedSessionOperation(h.sessionID, "atomic_model_input_batch")
		}
		return bindingStore.AppendItemsWithBinding(ctx, h.sessionID, "run:"+id, items, originalAgent, modelRef, activeAgent)
	}, func() {
		if modelRef != "" {
			d.sess.SetAgentModelOverride(originalAgent, modelRef)
		}
		d.sess.AgentName = activeAgent
		if modelRef != "" || activeAgent != originalAgent {
			d.modelRef, d.modelProviders = activeRef, slices.Clone(providers)
			d.bindingVersion++
		}
		d.authorizeViewLocked()
	})
	if err != nil {
		return Submission{}, err
	}
	d.WakePending()
	return Submission{SessionID: h.sessionID, TurnID: id}, nil
}

func (h *sessionHandle) QueueLegacyFollowUps(ctx context.Context, inputs []TurnInput, requestID string) (LegacyFollowUpResult, error) {
	id, err := sessionInputID(h.sessionID, requestID)
	if err != nil {
		return LegacyFollowUpResult{}, err
	}
	items := make([]session.Item, 0, len(inputs))
	for i, input := range inputs {
		if input.Retry {
			return LegacyFollowUpResult{}, errors.New("legacy followup cannot request retry")
		}
		msg := session.UserMessage(input.Content, input.MultiContent...)
		msg.Message.CreatedAt, msg.Pending, msg.Accepted = "", true, true
		msg.TurnID, msg.InputOrigin, msg.InputMode = id+":"+strconv.Itoa(i), session.InputOriginUser, "legacy_followup"
		items = append(items, session.NewMessageItem(msg))
	}
	var streaming bool
	_ = h.driver.ownerCall(ctx, func() error { streaming = h.driver.running() || h.driver.starting() || h.driver.settling(); return nil })
	duplicate, err := h.driver.legacyBatch(ctx, "followup:"+id, items, len(inputs), false, nil, func(ctx context.Context, store session.ItemBatchAppender) (session.ItemBatchReceipt, error) {
		return store.AppendItems(ctx, h.sessionID, "followup:"+id, items)
	}, nil)
	return LegacyFollowUpResult{Streaming: streaming, Duplicate: duplicate}, err
}

// legacyBatch preserves atomic store batches while the owner remains available
// during lookup, command evaluation, append and uncertain-ack reconciliation.
func (d *sessionDriver) legacyBatch(ctx context.Context, key string, identity []session.Item, count int, run bool, prepare func(*session.Session) error, appendBatch func(context.Context, session.ItemBatchAppender) (session.ItemBatchReceipt, error), publish func()) (duplicate bool, err error) {
	err = d.durableIO(ctx, func() (sessionIOReservation, error) {
		if err := d.admitLocked(SessionOperationPost); err != nil {
			return sessionIOReservation{}, err
		}
		store, ok := d.r.sessionStore.(session.ItemBatchAppender)
		if !ok {
			return sessionIOReservation{}, UnsupportedSessionOperation(d.identityID, "atomic_input_batch")
		}
		snapshot := d.sess.OwnSnapshot()
		busy := run && (d.running() || d.starting() || d.settling() || d.compactReserved || d.skillOperationID != "")
		capacity := d.pendingLimit() >= 0 && count > d.pendingLimit()-len(d.pending)
		if run {
			capacity = !limitAllows(len(d.pending), d.pendingLimit())
		}
		d.switchReserved = run
		var receipt session.ItemBatchReceipt
		var stored *session.Session
		var ref string
		var providers []provider.Provider
		return sessionIOReservation{write: func(ctx context.Context) error {
			var err error
			var found bool
			receipt, found, err = store.LookupItemBatch(ctx, d.identityID, key, identity)
			if err != nil {
				return err
			}
			duplicate = found
			if !found {
				if busy {
					return &SessionError{Kind: SessionErrorConflict, Operation: "legacy_run", Reason: SessionErrorReasonBusy}
				}
				if capacity {
					return ErrSessionCapacity
				}
				if prepare != nil {
					if err := prepare(snapshot); err != nil {
						return err
					}
				}
				receipt, err = appendBatch(ctx, store)
				if err != nil {
					return err
				}
				duplicate = receipt.Duplicate
			}
			stored, err = d.r.sessionStore.GetSession(ctx, d.identityID)
			if err != nil {
				return err
			}
			if run {
				ref, providers, err = d.r.resolveSessionModelBinding(ctx, stored, "")
			}
			return err
		}, commit: func(err error) error {
			d.switchReserved = false
			if err != nil {
				return err
			}
			present := map[int64]bool{}
			for _, item := range d.sess.MessagesSnapshot() {
				if item.Message != nil {
					present[item.Message.ID] = true
				}
			}
			wanted := map[int64]bool{}
			for _, id := range receipt.IDs {
				wanted[id] = true
			}
			for _, item := range stored.MessagesSnapshot() {
				msg := item.Message
				if msg == nil || present[msg.ID] || !wanted[msg.ID] {
					continue
				}
				pos := d.sess.AddMessageAt(msg)
				if msg.Pending && msg.Accepted {
					queued := queuedSessionInput(msg, pos, true)
					if msg.InputMode == "legacy_run" {
						d.pending = append([]QueuedMessage{queued}, d.pending...)
					} else {
						d.pending = append(d.pending, queued)
						d.events.PublishForRequest(d.identityID, queued.RequestID, inputEventMetadata(PendingUserMessageAccepted(d.identityID, queued.RequestID, queued.Content, queued.MultiContent, pos), queued))
					}
				}
			}
			if run {
				d.sess.AgentName = stored.AgentName
				overrides, custom := stored.ModelStateSnapshot()
				d.sess.ReplaceModelState(overrides, custom)
				d.modelRef, d.modelProviders = ref, providers
				d.bindingVersion++
			}
			if !duplicate && publish != nil {
				publish()
			}
			return nil
		}}, nil
	})
	if err != nil {
		return false, err
	}
	return duplicate, nil
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

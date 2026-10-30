package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"sync/atomic"

	"github.com/docker/docker-agent/pkg/agent"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/team"
	"github.com/docker/docker-agent/pkg/tools"
)

type sessionOwnerCommand struct {
	ctx   context.Context //nolint:containedctx // transient command carries caller cancellation until owner acknowledgement
	apply func() error
	done  chan error
	state atomic.Uint32
}

type executionIdentity struct {
	driver      *sessionDriver
	generation  uint64
	environment *TeamEnvironmentLease
}

type (
	executionIdentityKey  struct{}
	executionEventSinkKey struct{}
)

// ownerCall runs state-only transitions. External I/O and user callbacks belong
// to workers, whose completion must revalidate its reservation here.
func (d *sessionDriver) ownerCall(ctx context.Context, apply func() error) error {
	select {
	case <-d.ownerDone:
		return d.retiredOwnerError()
	default:
	}
	command := &sessionOwnerCommand{ctx: ctx, apply: apply, done: make(chan error, 1)}
	select {
	case d.ownerCommands <- command:
	case <-ctx.Done():
		return ctx.Err()
	case <-d.ownerDone:
		return d.retiredOwnerError()
	}
	await := func(cause error) error {
		if command.state.CompareAndSwap(0, 2) {
			return cause
		}
		// A started command contains state-only work. Joining it transfers all
		// closure outputs back to the caller without races after cancellation.
		return <-command.done
	}
	select {
	case err := <-command.done:
		return err
	case <-ctx.Done():
		return await(ctx.Err())
	case <-d.ownerDone:
		return await(d.retiredOwnerError())
	}
}

func (d *sessionDriver) retiredOwnerError() error {
	if d.registrySnapshot().stopped {
		return ErrSessionStopped
	}
	return ErrSessionClosed
}

func (d *sessionDriver) runOwner() {
	defer close(d.ownerDone)
	for {
		select {
		case command := <-d.ownerCommands:
			if !command.state.CompareAndSwap(0, 1) {
				continue
			}
			if err := command.ctx.Err(); err != nil {
				command.done <- err
				continue
			}
			d.mu.Lock()
			err := command.apply()
			d.publishRegistryStateLocked()
			d.mu.Unlock()
			command.done <- err
		case <-d.treeWake:
			d.mu.Lock()
			d.applyTreeProjectionLocked()
			d.mu.Unlock()
		case <-d.stopWake:
			d.mu.Lock()
			d.applyStopLocked()
			d.publishRegistryStateLocked()
			d.mu.Unlock()
		case <-d.ownerStop:
			return
		}
	}
}

func (d *sessionDriver) ownerSnapshot(ctx context.Context) (*session.Session, error) {
	var snapshot *session.Session
	err := d.ownerCall(ctx, func() error {
		if d.sess == nil {
			return ErrSessionClosed
		}
		snapshot = d.sess.Clone()
		return nil
	})
	return snapshot, err
}

func (d *sessionDriver) executionContext(ctx context.Context, generation uint64) (context.Context, *session.Session, error) {
	var snapshot *session.Session
	var environment *TeamEnvironmentLease
	err := d.ownerCall(ctx, func() error {
		if d.sess == nil {
			return ErrSessionClosed
		}
		if d.generation != generation {
			return ErrSessionStopped
		}
		snapshot = d.sess.Clone()
		environment = d.runEnvironment
		return nil
	})
	identity := executionIdentity{driver: d, generation: generation, environment: environment}
	ctx = context.WithValue(tools.WithResourceOwner(ctx, d.resourceOwner), executionIdentityKey{}, identity)
	if environment != nil {
		ctx = context.WithValue(ctx, teamEnvironmentContextKey{}, environment.Environment())
		ctx = agent.WithContextModels(ctx, snapshot.AgentName, identity.driver.activeBinding().models)
	}
	return ctx, snapshot, err
}

// commitExecutionEvent applies only the event's typed delta; concurrent accepted
// input and edits never get replaced by an execution worker's private snapshot.
func (d *sessionDriver) commitExecutionEvent(ctx context.Context, generation uint64, event Event) error {
	return d.ownerCall(ctx, func() error {
		if generation != d.generation || generation <= d.settledGeneration || d.sess == nil {
			return &SessionError{Kind: SessionErrorStale, SessionID: d.identityID, Operation: "execution_effect"}
		}
		if scoped, ok := event.(SessionScoped); ok && scoped.GetSessionID() != d.identityID {
			return nil
		}
		switch effect := event.(type) {
		case *UserMessageEvent:
			if effect.TurnID != "" || !effect.ownerAppend || effect.SessionID != d.identityID {
				return nil
			}
			msg := QueuedMessage{Content: effect.Message, MultiContent: effect.MultiContent, InputOrigin: effect.InputOrigin, SenderID: effect.SenderID, SenderName: effect.SenderName, ReportOutcome: effect.ReportOutcome, InputMode: effect.InputMode}.sessionMessage()
			data, err := json.Marshal(msg)
			if err != nil {
				return err
			}
			message := &session.Message{}
			if err := json.Unmarshal(data, message); err != nil {
				return err
			}
			effect.SessionPosition = d.sess.AddMessageAt(message)
			effect.ownerMessage = message
		case *MessageAddedEvent:
			if effect.boundaryOnly || effect.Message == nil {
				return nil
			}
			data, err := json.Marshal(effect.Message)
			if err != nil {
				return err
			}
			message := &session.Message{}
			if err := json.Unmarshal(data, message); err != nil {
				return err
			}
			message.ID = effect.Message.ID
			effect.SessionPosition = d.sess.AddMessageAt(message)
			effect.ownerMessage = message
			if message.Message.Role == "assistant" {
				d.generationResult = message.Message.Content
			}
		case *TokenUsageEvent:
			if effect.Usage != nil {
				d.sess.SetTokensAndCost(effect.Usage.InputTokens, effect.Usage.OutputTokens, effect.Usage.Cost)
			}
		case *SessionTitleEvent:
			d.sess.SetTitle(effect.Title)
		case *SessionSummaryEvent:
			if !effect.persisted {
				item := session.Item{Summary: effect.Summary, FirstKeptEntry: effect.FirstKeptEntry, Cost: effect.Cost, Model: effect.Model, Usage: effect.Usage}
				d.sess.ApplyCompaction(0, 0, item)
			}
		}
		return nil
	})
}

func refreshExecutionInput(ctx context.Context, scratch *session.Session) error {
	identity, ok := ctx.Value(executionIdentityKey{}).(executionIdentity)
	if !ok {
		return nil
	}
	var fresh *session.Session
	err := identity.driver.ownerCall(ctx, func() error {
		if identity.generation != identity.driver.generation || identity.driver.sess == nil {
			return ErrSessionStopped
		}
		fresh = identity.driver.sess.Clone()
		return nil
	})
	if err != nil {
		return err
	}
	known := map[string]bool{}
	for _, item := range scratch.MessagesSnapshot() {
		if item.Message != nil && item.Message.TurnID != "" {
			known[item.Message.TurnID] = true
		}
	}
	for _, item := range fresh.Messages {
		message := item.Message
		if message != nil && message.Accepted && !message.Pending {
			if known[message.TurnID] {
				scratch.PromotePendingUserMessageByTurnID(message.TurnID)
			} else {
				scratch.AddMessageAt(message)
			}
		}
	}
	approved, policy, permissions := fresh.SafetySettings()
	scratch.SetSafetyPolicy(policy)
	scratch.SetToolsApproved(approved)
	scratch.SetPermissions(permissions)
	return nil
}

func (r *LocalRuntime) commitActiveAgent(ctx context.Context, scratch *session.Session, name string) error {
	identity, ok := ctx.Value(executionIdentityKey{}).(executionIdentity)
	if !ok {
		return ErrSessionClosed
	}
	d := identity.driver
	tm := r.teamForContext(ctx, scratch)
	if tm == nil {
		return ErrSessionClosed
	}
	var nextBinding *session.Session
	var binding activeSessionBinding
	if err := d.ownerCall(ctx, func() error {
		if d.stopped || d.generation != identity.generation {
			return ErrSessionStopped
		}
		nextBinding = d.sess.OwnSnapshot()
		nextBinding.AgentName = name
		nextBinding.AgentID = configuredAgentID(tm, name)
		binding = activeSessionBinding{version: d.bindingVersion, agentName: d.AgentNameLocked()}
		return nil
	}); err != nil {
		return err
	}
	modelRef := ""
	if nextBinding.AgentModelOverrides != nil {
		modelRef = nextBinding.AgentModelOverrides[name]
	}
	ref, models, err := r.resolveSessionModelBinding(ctx, nextBinding, modelRef)
	if err != nil {
		return err
	}
	err = d.durableIO(ctx, func() (sessionIOReservation, error) {
		if d.stopped || d.generation != identity.generation {
			return sessionIOReservation{}, ErrSessionStopped
		}
		if err := d.validateBindingLocked(binding); err != nil {
			return sessionIOReservation{}, err
		}
		next := d.sess.OwnSnapshot()
		next.AgentName = name
		next.AgentID = configuredAgentID(tm, name)
		return sessionIOReservation{
			write: func(ctx context.Context) error {
				if r.sessionStore != nil {
					return r.sessionStore.UpdateSession(ctx, next)
				}
				return nil
			},
			commit: func(err error) error {
				if err != nil {
					return err
				}
				if d.generation != identity.generation || d.stopped {
					return ErrSessionStopped
				}
				if err := d.validateBindingLocked(binding); err != nil {
					return err
				}
				d.sess.AgentName = name
				d.sess.AgentID = configuredAgentID(tm, name)
				d.modelProviders = models
				d.modelRef = ref
				d.bindingVersion++
				rebindExecutionBudget(ctx, name)
				return nil
			},
		}, nil
	})
	if err == nil {
		scratch.AgentName = name
		scratch.AgentID = configuredAgentID(tm, name)
	}
	return err
}

func (r *LocalRuntime) commitApproval(ctx context.Context, scratch *session.Session, request ResumeRequest, toolName string) error {
	identity, ok := ctx.Value(executionIdentityKey{}).(executionIdentity)
	if !ok {
		return nil
	}
	apply := func(sess *session.Session) {
		switch NormalizeResumeType(request.Type) {
		case ResumeTypeApproveBalanced:
			sess.SetSafetyPolicy(session.SafetyPolicyBalanced)
		case ResumeTypeApproveAutonomous:
			sess.SetSafetyPolicy(session.SafetyPolicyAutonomous)
		case ResumeTypeApproveTool:
			name := request.ToolName
			if name == "" {
				name = toolName
			}
			sess.AppendPermissionAllow(name)
		}
	}
	switch NormalizeResumeType(request.Type) {
	case ResumeTypeApproveBalanced, ResumeTypeApproveAutonomous, ResumeTypeApproveTool:
	default:
		return nil
	}
	d := identity.driver
	return d.durableIO(ctx, func() (sessionIOReservation, error) {
		if d.stopped || d.generation != identity.generation {
			return sessionIOReservation{}, ErrSessionStopped
		}
		next := d.sess.OwnSnapshot()
		apply(next)
		return sessionIOReservation{
			write: func(ctx context.Context) error {
				if r.sessionStore != nil {
					return r.sessionStore.UpdateSession(ctx, next)
				}
				return nil
			},
			commit: func(err error) error {
				if err != nil {
					return err
				}
				if d.stopped || d.generation != identity.generation {
					return ErrSessionStopped
				}
				apply(d.sess)
				return nil
			},
		}, nil
	})
}

type compactionIdentity struct {
	driver    *sessionDriver
	operation uint64
}
type compactionIdentityKey struct{}

func (r *LocalRuntime) persistCompactionEffect(ctx context.Context, scratch *session.Session, inputTokens int64, item session.Item) error {
	identity, executing := ctx.Value(executionIdentityKey{}).(executionIdentity)
	manual, compacting := ctx.Value(compactionIdentityKey{}).(compactionIdentity)
	var d *sessionDriver
	switch {
	case executing:
		d = identity.driver
	case compacting:
		d = manual.driver
	default:
		if r.sessionStore != nil {
			return r.sessionStore.PersistCompaction(ctx, scratch, inputTokens, 0, item)
		}
		scratch.ApplyCompaction(inputTokens, 0, item)
		return nil
	}
	valid := func() bool {
		return !d.stopped && ((executing && d.generation == identity.generation) || (compacting && d.compactOperation == manual.operation))
	}
	err := d.durableIO(ctx, func() (sessionIOReservation, error) {
		if !valid() {
			return sessionIOReservation{}, ErrSessionStopped
		}
		next := d.sess.OwnSnapshot()
		return sessionIOReservation{
			write: func(ctx context.Context) error {
				if r.sessionStore != nil {
					return r.sessionStore.PersistCompaction(ctx, next, inputTokens, 0, item)
				}
				next.ApplyCompaction(inputTokens, 0, item)
				return nil
			},
			commit: func(err error) error {
				if err != nil {
					return err
				}
				if !valid() {
					return ErrSessionStopped
				}
				d.sess.ApplyCompaction(inputTokens, 0, item)
				return nil
			},
		}, nil
	})
	if err == nil {
		scratch.ApplyCompaction(inputTokens, 0, item)
	}
	return err
}

func (d *sessionDriver) registerElicitation(ctx context.Context, id string, event *ElicitationRequestEvent, waiter *elicitationWaiter) error {
	detached, err := observerEventSnapshot(event)
	if err != nil {
		return err
	}
	event = detached.(*ElicitationRequestEvent)
	return d.ownerCall(ctx, func() error {
		if d.stopped {
			return ErrSessionStopped
		}
		if identity, ok := ctx.Value(executionIdentityKey{}).(executionIdentity); ok && (identity.driver != d || identity.generation != d.generation) {
			return ErrSessionStopped
		}
		d.interactions[id] = sessionInteraction{kind: InteractionElicitation, turnID: d.activeRequestID, generation: d.generation, event: event, waiter: waiter}
		d.events.Publish(d.identityID, event)
		return nil
	})
}

func (r *LocalRuntime) commitSessionAttribute(ctx context.Context, scratch *session.Session, key, value string) error {
	identity, ok := ctx.Value(executionIdentityKey{}).(executionIdentity)
	if !ok {
		scratch.SetAttribute(key, value)
		return nil
	}
	d := identity.driver
	err := d.durableIO(ctx, func() (sessionIOReservation, error) {
		if d.stopped || identity.generation != d.generation {
			return sessionIOReservation{}, ErrSessionStopped
		}
		next := d.sess.OwnSnapshot()
		next.SetAttribute(key, value)
		return sessionIOReservation{
			write: func(ctx context.Context) error {
				if r.sessionStore != nil {
					return r.sessionStore.UpdateSession(ctx, next)
				}
				return nil
			},
			commit: func(err error) error {
				if err != nil {
					return err
				}
				if d.stopped || identity.generation != d.generation {
					return ErrSessionStopped
				}
				d.sess.SetAttribute(key, value)
				return nil
			},
		}, nil
	})
	if err == nil {
		scratch.SetAttribute(key, value)
	}
	return err
}

func (d *sessionDriver) closeOwner() {
	d.ownerClose.Do(func() { close(d.ownerStop) })
	<-d.ownerDone
	d.mu.Lock()
	pinned, running := d.teamEnvironment, d.runEnvironment
	d.teamEnvironment, d.runEnvironment = nil, nil
	d.mu.Unlock()
	if running != nil {
		_ = running.Release(context.WithoutCancel(d.r.lifetime()))
	}
	if pinned != nil && pinned != running {
		_ = pinned.Release(context.WithoutCancel(d.r.lifetime()))
	}
}

func (d *sessionDriver) abandonElicitation(id string, waiter *elicitationWaiter) {
	_ = d.ownerCall(context.WithoutCancel(d.r.lifetime()), func() error {
		if interaction, ok := d.interactions[id]; ok && interaction.waiter == waiter {
			if !waiter.tryCancel() && waiterState(waiter.state.Load()) != waiterCanceled {
				return nil
			}
			delete(d.interactions, id)
			reason := InteractionCanceled
			if d.stopped {
				reason = InteractionStopped
			}
			d.events.Publish(d.identityID, &InteractionResolvedEvent{Type: "interaction_resolved", SessionID: d.identityID, InteractionID: id, Reason: reason})
		}
		return nil
	})
	d.refreshAttention()
}

func (r *LocalRuntime) runOwnerQueuedCompaction(ctx context.Context, scratch *session.Session) {
	identity, ok := ctx.Value(executionIdentityKey{}).(executionIdentity)
	if !ok {
		return
	}
	d := identity.driver
	var request *liveCompactionRequest
	var operation uint64
	if err := d.ownerCall(context.WithoutCancel(ctx), func() error {
		if d.generation != identity.generation {
			return ErrSessionStopped
		}
		request = d.queuedCompaction
		d.queuedCompaction = nil
		operation = d.compactOperation
		return nil
	}); err != nil || request == nil {
		return
	}
	compactionCtx := context.WithValue(ctx, compactionIdentityKey{}, compactionIdentity{d, operation})
	r.runLiveCompactionRequest(compactionCtx, scratch, *request)
	_ = d.ownerCall(context.WithoutCancel(ctx), func() error {
		if d.compactOperation == operation {
			d.compactReserved = false
			d.refreshSteeringLocked()
		}
		return nil
	})
}

func (d *sessionDriver) registerResumeInteraction(ctx context.Context, id string, kind InteractionKind, event Event) (<-chan ResumeRequest, error) {
	payload, err := observerEventSnapshot(event)
	if err != nil {
		return nil, err
	}
	var channel chan ResumeRequest
	err = d.ownerCall(ctx, func() error {
		if d.stopped {
			return ErrSessionStopped
		}
		if identity, ok := ctx.Value(executionIdentityKey{}).(executionIdentity); ok && (identity.driver != d || identity.generation != d.generation) {
			return ErrSessionStopped
		}
		if _, exists := d.interactions[id]; exists {
			return &SessionError{Kind: SessionErrorConflict, SessionID: d.identityID, RequestID: id, Operation: "register_interaction"}
		}
		channel = make(chan ResumeRequest, 1)
		d.interactions[id] = sessionInteraction{kind: kind, turnID: d.activeRequestID, generation: d.generation, resume: channel, event: payload}
		d.events.Publish(d.identityID, payload)
		return nil
	})
	d.refreshAttention()
	return channel, err
}

func (d *sessionDriver) registerResume(ctx context.Context, id string) (<-chan ResumeRequest, error) {
	var channel chan ResumeRequest
	err := d.ownerCall(ctx, func() error {
		if d.stopped {
			return ErrSessionStopped
		}
		if identity, ok := ctx.Value(executionIdentityKey{}).(executionIdentity); ok && (identity.driver != d || identity.generation != d.generation) {
			return ErrSessionStopped
		}
		if _, exists := d.interactions[id]; exists {
			return &SessionError{Kind: SessionErrorConflict, SessionID: d.identityID, RequestID: id, Operation: "register_interaction"}
		}
		channel = make(chan ResumeRequest, 1)
		d.interactions[id] = sessionInteraction{kind: InteractionConfirmation, turnID: d.activeRequestID, generation: d.generation, resume: channel}
		return nil
	})
	return channel, err
}

func (r *LocalRuntime) recordElicitationDecline(ctx context.Context, sessionID, question string) {
	if r.sessionDrivers == nil || sessionID == "" {
		return
	}
	d, ok := r.sessionDrivers.Lookup(sessionID)
	if !ok {
		return
	}
	var message *session.Message
	err := d.durableIO(ctx, func() (sessionIOReservation, error) {
		if d.stopped {
			return sessionIOReservation{}, ErrSessionStopped
		}
		message = &session.Message{Implicit: true, InputOrigin: session.InputOriginRuntime, InputMode: "elicitation_declined", Message: chat.Message{Role: chat.MessageRoleSystem, Content: backgroundElicitationDeclinedNote(question)}}
		var rowID int64
		return sessionIOReservation{
			write: func(ctx context.Context) error {
				if r.sessionStore == nil {
					return nil
				}
				var err error
				rowID, err = r.sessionStore.AddMessage(ctx, sessionID, message)
				return err
			},
			commit: func(err error) error {
				if err != nil {
					return err
				}
				if d.stopped {
					return ErrSessionStopped
				}
				message.ID = rowID
				position := d.sess.AddMessageAt(message)
				d.events.Publish(sessionID, MessageAddedAt(sessionID, message, d.AgentNameLocked(), position))
				return nil
			},
		}, nil
	})
	if err != nil {
		return
	}
	if sink, ok := ctx.Value(executionEventSinkKey{}).(EventSink); ok {
		sink.Emit(Warning(message.Message.Content, d.AgentName()))
	}
}

// retire keeps failed settlement authoritative and closes the owner only after drain.
// Contract: the stop wait inside drain (stopAllRetiring, awaitStop) observes the drain
// context and owner closure, so the retirement gate is released once either occurs.
// closeOwner still waits on ownerDone, which the owner closes promptly between
// commands. Cleanup runs on its own goroutine and cannot block the owner. A user stop
// that ignores its context keeps only its own goroutine and the driver's work group
// busy. Callers run retirement on a background goroutine and never under a registry,
// shard, transition, service, or driver lock.
func (d *sessionDriver) retire(ctx context.Context) error {
	if err := d.acquireRetirement(ctx); err != nil {
		return err
	}
	defer d.releaseRetirement()
	if err := d.drainRetirementOwned(ctx); err != nil {
		return err
	}
	d.closeOwner()
	return nil
}

func (d *sessionDriver) acquireRetirement(ctx context.Context) error {
	select {
	case d.retirementGate <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (d *sessionDriver) releaseRetirement() { <-d.retirementGate }

func (d *sessionDriver) drainRetirement(ctx context.Context) error {
	if err := d.acquireRetirement(ctx); err != nil {
		return err
	}
	defer d.releaseRetirement()
	return d.drainRetirementOwned(ctx)
}

func (d *sessionDriver) drainRetirementOwned(ctx context.Context) error {
	select {
	case <-d.ownerDone:
		return nil
	default:
	}
	ctx, cancel := context.WithTimeout(ctx, defaultSubagentPersistenceTimeout)
	defer cancel()
	d.stopAllRetiring(ctx)
	select {
	case <-d.Done():
	case <-ctx.Done():
		return ctx.Err()
	}
	d.mu.Lock()
	settling, generation, runErr := d.settling(), d.generation, d.completionRunErr
	d.mu.Unlock()
	if settling {
		d.finishRunContext(ctx, generation, runErr)
		d.mu.Lock()
		err := d.completionErr
		d.mu.Unlock()
		if err != nil {
			return err
		}
	}
	if err := d.ownerCall(ctx, func() error {
		d.ioSealed = true
		return nil
	}); err != nil {
		return err
	}
	select {
	case <-d.Done():
	case <-ctx.Done():
		return ctx.Err()
	}
	cleanupErr := d.cleanupResources(ctx)
	select {
	case <-d.Done():
		return cleanupErr
	case <-ctx.Done():
		return errors.Join(cleanupErr, ctx.Err())
	}
}

type resourceCleanupAttempt struct {
	done chan struct{}
	err  error
}

func (d *sessionDriver) cleanupResources(ctx context.Context) error {
	var attempt *resourceCleanupAttempt
	var start bool
	var toolsets []tools.ToolSet
	if err := d.ownerCall(ctx, func() error {
		attempt = d.resourceCleanup
		if attempt != nil {
			select {
			case <-attempt.done:
				if attempt.err == nil {
					return nil
				}
			default:
				return nil
			}
		}
		attempt = &resourceCleanupAttempt{done: make(chan struct{})}
		d.resourceCleanup = attempt
		d.wg.Add(1)
		start = true
		if d.sess != nil {
			toolsets = append(toolsets, d.sess.ExtraToolSets...)
		}
		return nil
	}); err != nil {
		return err
	}
	if start {
		cleanupCtx, cancel := context.WithTimeout(tools.WithResourceOwner(ctx, d.resourceOwner), defaultSubagentPersistenceTimeout)
		go func() {
			defer d.wg.Done()
			defer cancel()
			var errs []error
			if d.r != nil {
				teams := []*team.Team{}
				if environment := d.teamEnvironmentSnapshot(); environment != nil {
					teams = append(teams, environment.Team())
				} else if d.r.teamEnvironments == nil && d.r.team != nil {
					teams = append(teams, d.r.team)
				}
				if d.r.teamEnvironments != nil {
					for _, env := range d.r.teamEnvironments.SnapshotEnvironments() {
						teams = append(teams, env.Team())
					}
				}
				for _, tm := range teams {
					if tm == nil {
						continue
					}
					for _, name := range tm.AgentNames() {
						if a, err := tm.Agent(name); err == nil {
							toolsets = append(toolsets, a.ToolSets()...)
						}
					}
				}
			}
			errs = append(errs, tools.StopResourceOwners(cleanupCtx, toolsets...))
			_ = d.ownerCall(context.WithoutCancel(ctx), func() error {
				attempt.err = errors.Join(errs...)
				close(attempt.done)
				return nil
			})
		}()
	}
	select {
	case <-attempt.done:
		return attempt.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// teamForSession returns the team revision pinned to the owning session. A
// driver retains that lease until it atomically adopts a later revision at a
// turn boundary, so callers never observe a disposed environment.
func (r *LocalRuntime) teamForSession(sess *session.Session) *team.Team {
	if sess != nil && r.sessionDrivers != nil {
		if driver, ok := r.sessionDrivers.Lookup(sess.ID); ok {
			if env := driver.teamEnvironmentSnapshot(); env != nil {
				return env.Team()
			}
		}
	}
	if r.teamEnvironments != nil {
		return nil
	}
	return r.team
}

func (d *sessionDriver) teamEnvironmentSnapshot() *TeamEnvironment {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.teamEnvironment == nil {
		return nil
	}
	return d.teamEnvironment.Environment()
}

func (r *LocalRuntime) envFromContext(ctx context.Context) *TeamEnvironment {
	if ctx == nil {
		return nil
	}
	if id, ok := ctx.Value(executionIdentityKey{}).(executionIdentity); ok && id.environment != nil {
		return id.environment.Environment()
	}
	return nil
}

func (r *LocalRuntime) teamForContext(ctx context.Context, sess *session.Session) *team.Team {
	if r.teamEnvironments != nil {
		if env := teamEnvironmentFromContext(ctx); env != nil {
			return env.Team()
		}
		if env := r.envFromContext(ctx); env != nil {
			return env.Team()
		}
		if sess != nil && r.sessionDrivers != nil {
			if d, ok := r.sessionDrivers.Lookup(sess.ID); ok {
				if env := d.teamEnvironmentSnapshot(); env != nil {
					return env.Team()
				}
			}
		}
		return nil
	}
	return r.teamForSession(sess)
}

func (d *sessionDriver) pinCurrentTeamEnvironment(ctx context.Context) error {
	if d.r.teamEnvironments == nil {
		return nil
	}
	lease, err := d.r.teamEnvironments.AcquireCurrent()
	if err != nil {
		return err
	}
	next := d.session()
	if next == nil {
		_ = lease.Release(context.WithoutCancel(ctx))
		return ErrSessionClosed
	}
	tm := lease.Environment().Team()
	if creation, _ := next.Attribute(SessionAgentIDAttribute); creation != "" {
		if _, _, exists := tm.ResolveIdentity(creation); !exists {
			_ = lease.Release(context.WithoutCancel(ctx))
			return &SessionError{Kind: SessionErrorNotFound, SessionID: next.ID, Operation: "resolve_creation_agent_id"}
		}
	} else if creation, _ := next.Attribute(SessionAgentAttribute); creation != "" {
		if _, _, exists := tm.ResolveLegacyName(creation); !exists {
			_ = lease.Release(context.WithoutCancel(ctx))
			return &SessionError{Kind: SessionErrorNotFound, SessionID: next.ID, Operation: "resolve_creation_agent_name"}
		}
	}
	var active *agent.Agent
	var activeName string
	var ok bool
	if next.AgentID != "" {
		active, activeName, ok = tm.ResolveIdentity(next.AgentID)
	} else if next.AgentName != "" {
		active, activeName, ok = tm.ResolveLegacyName(next.AgentName)
	} else {
		active, err = tm.DefaultAgent()
		if err == nil {
			activeName, ok = active.Name(), true
		}
	}
	if !ok || active == nil {
		_ = lease.Release(context.WithoutCancel(ctx))
		return &SessionError{Kind: SessionErrorNotFound, SessionID: next.ID, Operation: "resolve_active_agent_id"}
	}
	next.AgentName, next.AgentID, next.MaxIterations = activeName, configuredAgentID(tm, activeName), active.MaxIterations()
	envCtx := context.WithValue(ctx, teamEnvironmentContextKey{}, lease.Environment())
	modelRef, models, err := d.r.resolveSessionModelBinding(envCtx, next, "")
	if err != nil {
		_ = lease.Release(context.WithoutCancel(ctx))
		return err
	}
	var old *TeamEnvironmentLease
	err = d.ownerCall(ctx, func() error {
		if d.stopped || d.sess == nil {
			return ErrSessionStopped
		}
		if d.teamEnvironment != nil && d.teamEnvironment.Environment() == lease.Environment() {
			return nil
		}
		old, d.teamEnvironment = d.teamEnvironment, lease
		d.sess.AgentName, d.sess.AgentID, d.sess.MaxIterations = next.AgentName, next.AgentID, next.MaxIterations
		d.modelRef, d.modelProviders = modelRef, slices.Clone(models)
		d.bindingVersion++
		return nil
	})
	if err != nil {
		_ = lease.Release(context.WithoutCancel(ctx))
		return err
	}
	d.mu.Lock()
	installed := d.teamEnvironment == lease
	d.mu.Unlock()
	if !installed {
		_ = lease.Release(context.WithoutCancel(ctx))
		return nil
	}
	if old != nil {
		_ = old.Release(context.WithoutCancel(ctx))
	}
	return nil
}

func (d *sessionDriver) acquireRunEnvironment(ctx context.Context, generation uint64, continuation bool) error {
	if d.r.teamEnvironments == nil {
		return nil
	}
	if !continuation {
		if err := d.pinCurrentTeamEnvironment(ctx); err != nil {
			return err
		}
	}
	var lease *TeamEnvironmentLease
	err := d.ownerCall(ctx, func() error {
		if d.generation != generation || d.stopped {
			return ErrSessionStopped
		}
		if d.teamEnvironment == nil {
			return errors.New("session has no pinned team environment")
		}
		lease = d.teamEnvironment
		return nil
	})
	if err != nil {
		return err
	}
	runLease, err := d.r.teamEnvironments.Acquire(lease.Environment().RevisionID())
	if err != nil {
		return err
	}
	var previous *TeamEnvironmentLease
	err = d.ownerCall(ctx, func() error {
		if d.generation != generation || d.stopped {
			return ErrSessionStopped
		}
		previous, d.runEnvironment = d.runEnvironment, runLease
		return nil
	})
	if err != nil {
		_ = runLease.Release(context.WithoutCancel(ctx))
		return err
	}
	if previous != nil {
		_ = previous.Release(context.WithoutCancel(ctx))
	}
	return nil
}

func (d *sessionDriver) releaseRunEnvironment(ctx context.Context, generation uint64) {
	if d.r.teamEnvironments == nil {
		return
	}
	var lease *TeamEnvironmentLease
	_ = d.ownerCall(context.WithoutCancel(ctx), func() error {
		if generation == d.generation {
			lease, d.runEnvironment = d.runEnvironment, nil
		}
		return nil
	})
	if lease != nil {
		_ = lease.Release(context.WithoutCancel(ctx))
	}
}

type teamEnvironmentContextKey struct{}

func (d *sessionDriver) scopeTeamEnvironment(ctx context.Context) context.Context {
	var lease *TeamEnvironmentLease
	d.mu.Lock()
	lease = d.runEnvironment
	d.mu.Unlock()
	if lease == nil {
		return ctx
	}
	return context.WithValue(ctx, teamEnvironmentContextKey{}, lease.Environment())
}

func teamEnvironmentFromContext(ctx context.Context) *TeamEnvironment {
	if ctx == nil {
		return nil
	}
	environment, _ := ctx.Value(teamEnvironmentContextKey{}).(*TeamEnvironment)
	return environment
}

func environmentScopedContext(ctx context.Context, lease *TeamEnvironmentLease) context.Context {
	if lease == nil {
		return ctx
	}
	return context.WithValue(ctx, teamEnvironmentContextKey{}, lease.Environment())
}

func (d *sessionDriver) acquireOperationEnvironment(ctx context.Context) (context.Context, func(), error) {
	if d.r.teamEnvironments == nil {
		return ctx, func() {}, nil
	}
	var pinned *TeamEnvironmentLease
	if err := d.ownerCall(ctx, func() error { pinned = d.teamEnvironment; return nil }); err != nil {
		return nil, nil, err
	}
	if pinned == nil {
		return nil, nil, errors.New("session has no pinned team environment")
	}
	lease, err := d.r.teamEnvironments.Acquire(pinned.Environment().RevisionID())
	if err != nil {
		return nil, nil, err
	}
	scoped := context.WithValue(ctx, teamEnvironmentContextKey{}, lease.Environment())
	return scoped, func() { _ = lease.Release(context.WithoutCancel(ctx)) }, nil
}

// captureEnvironmentForOperation pins the selected session revision for a
// standalone operation that can outlive its call stack (title, skill, compaction,
// reports). In edit-enabled runtimes, missing session ownership is an error,
// never a fallback to the constructor Team.
func (r *LocalRuntime) captureEnvironmentForOperation(ctx context.Context, sess *session.Session) (context.Context, func(), error) {
	if r.teamEnvironments == nil {
		return ctx, func() {}, nil
	}
	if env := teamEnvironmentFromContext(ctx); env != nil {
		lease, err := r.teamEnvironments.Acquire(env.RevisionID())
		if err != nil {
			return nil, nil, err
		}
		scoped := context.WithValue(ctx, teamEnvironmentContextKey{}, lease.Environment())
		return scoped, func() { _ = lease.Release(context.WithoutCancel(ctx)) }, nil
	}
	if identity, ok := ctx.Value(executionIdentityKey{}).(executionIdentity); ok && identity.environment != nil {
		lease, err := r.teamEnvironments.Acquire(identity.environment.Environment().RevisionID())
		if err != nil {
			return nil, nil, err
		}
		scoped := context.WithValue(ctx, teamEnvironmentContextKey{}, lease.Environment())
		return scoped, func() { _ = lease.Release(context.WithoutCancel(ctx)) }, nil
	}
	if sess == nil || r.sessionDrivers == nil {
		return nil, nil, errors.New("operation has no pinned team environment")
	}
	driver, ok := r.sessionDrivers.Lookup(sess.ID)
	if !ok {
		return nil, nil, errors.New("operation session has no environment owner")
	}
	var pinned *TeamEnvironmentLease
	if err := driver.ownerCall(ctx, func() error { pinned = driver.teamEnvironment; return nil }); err != nil {
		return nil, nil, err
	}
	if pinned == nil {
		return nil, nil, errors.New("operation session has no pinned team environment")
	}
	lease, err := r.teamEnvironments.Acquire(pinned.Environment().RevisionID())
	if err != nil {
		return nil, nil, err
	}
	scoped := context.WithValue(ctx, teamEnvironmentContextKey{}, lease.Environment())
	return scoped, func() { _ = lease.Release(context.WithoutCancel(ctx)) }, nil
}

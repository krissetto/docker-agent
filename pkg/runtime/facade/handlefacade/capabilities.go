package handlefacade

import (
	"context"
	"errors"
	"fmt"

	"github.com/docker/docker-agent/pkg/effort"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/runtime/facade"
	"github.com/docker/docker-agent/pkg/runtime/modelcatalog"
)

// Each capability is a small value over the session so that the Session
// interface can expose accessors without clashing method names.

type queueCap struct{ s *sessionImpl }

func (c queueCap) Steer(ctx context.Context, input facade.Input) (facade.Ticket, error) {
	if _, err := c.s.ensureFeed(ctx); err != nil {
		return facade.Ticket{}, err
	}
	submission, err := c.s.handle.Steer(ctx, c.s.turnInput(ctx, input))
	if err != nil {
		return facade.Ticket{}, err
	}
	return facade.Ticket{ID: submission.TurnID}, nil
}

func (c queueCap) FollowUp(ctx context.Context, input facade.Input) (facade.Ticket, error) {
	if _, err := c.s.ensureFeed(ctx); err != nil {
		return facade.Ticket{}, err
	}
	submission, err := c.s.handle.Submit(ctx, c.s.turnInput(ctx, input))
	if err != nil {
		return facade.Ticket{}, err
	}
	return facade.Ticket{ID: submission.TurnID}, nil
}

type withdrawCap struct{ s *sessionImpl }

// Withdraw reports withdrawn when the runtime removed the entry, consumed when this
// session observed its consumption, and not_withdrawn otherwise.
func (c withdrawCap) Withdraw(ctx context.Context, ticket facade.Ticket) (facade.WithdrawOutcome, error) {
	canceler, ok := c.s.handle.(runtime.PendingMessageCanceler)
	if !ok {
		return "", fmt.Errorf("withdraw: %w", facade.ErrUnsupported)
	}
	f, err := c.s.ensureFeed(ctx)
	if err != nil {
		return "", err
	}
	withdrawn, err := canceler.CancelPendingMessage(ctx, ticket.ID)
	if err != nil {
		return "", err
	}
	if withdrawn {
		return facade.WithdrawWithdrawn, nil
	}
	if f.isConsumed(ticket.ID) {
		return facade.WithdrawConsumed, nil
	}
	return facade.WithdrawNotWithdrawn, nil
}

type pendingCap struct{ s *sessionImpl }

// List reports only entries whose durable acceptance has completed. The runtime
// queues an input before its transcript record is accepted, and Edit can act only
// on accepted records, so listing an unaccepted entry would advertise an edit that
// is refused.
func (c pendingCap) List(ctx context.Context) ([]facade.PendingInput, error) {
	observed, err := observeOnce(ctx, c.s.handle)
	if err != nil {
		return nil, err
	}
	transcript, err := c.s.handle.Snapshot(ctx)
	if err != nil {
		return nil, err
	}
	accepted := map[string]bool{}
	for _, msg := range transcript.GetAllMessages() {
		if msg.Pending && msg.Accepted {
			accepted[msg.TurnID] = true
		}
	}
	out := make([]facade.PendingInput, 0, len(observed.PendingInputs))
	for _, input := range observed.PendingInputs {
		if !accepted[input.TurnID] {
			continue
		}
		out = append(out, facade.PendingInput{
			Ticket:       facade.Ticket{ID: input.TurnID},
			Content:      input.Content,
			MultiContent: input.MultiContent,
		})
	}
	return out, nil
}

func (c pendingCap) Edit(ctx context.Context, ticket facade.Ticket, expected, content string) (facade.EditOutcome, error) {
	_, err := c.s.handle.Edit(ctx, runtime.SessionEdit{
		Kind: runtime.SessionEditPendingMessage,
		PendingMessage: &runtime.PendingMessageEdit{
			TurnID:          ticket.ID,
			Content:         content,
			ExpectedContent: &expected,
		},
	})
	var sessionErr *runtime.SessionError
	switch {
	case err == nil:
		return facade.EditEdited, nil
	case errors.As(err, &sessionErr) && sessionErr.Kind == runtime.SessionErrorStale:
		return facade.EditStale, nil
	default:
		return "", err
	}
}

type compactCap struct{ s *sessionImpl }

func (c compactCap) Compact(ctx context.Context, prompt string) error {
	return c.s.handle.Compact(ctx, prompt, nil)
}

type pauseCap struct{ s *sessionImpl }

func (c pauseCap) TogglePause(ctx context.Context) (bool, error) {
	return c.s.handle.TogglePause(ctx)
}

type treeCap struct{ s *sessionImpl }

func (c treeCap) Tree(ctx context.Context) (facade.Tree, error) {
	snapshot, err := c.s.handle.Snapshot(ctx)
	if err != nil {
		return facade.Tree{}, err
	}
	return convertTree(snapshot.GetSubagentTree()), nil
}

type stopCap struct{ s *sessionImpl }

func (c stopCap) StopSubtree(ctx context.Context) error {
	controller, ok := c.s.handle.(runtime.SessionTreeController)
	if !ok {
		return fmt.Errorf("stop subtree: %w", facade.ErrUnsupported)
	}
	return controller.StopSubtree(ctx)
}

type todoCap struct{ s *sessionImpl }

func (c todoCap) SetStatus(ctx context.Context, id, status string) ([]facade.Todo, error) {
	todos, err := c.s.handle.SetTodoStatus(ctx, id, status)
	if err != nil {
		return nil, err
	}
	return convertTodos(todos), nil
}

func (c todoCap) SetDescription(ctx context.Context, id, expectedDescription, description string) ([]facade.Todo, error) {
	todos, err := c.s.handle.SetTodoDescription(ctx, id, expectedDescription, description)
	if err != nil {
		return nil, err
	}
	return convertTodos(todos), nil
}

func (c todoCap) Remove(ctx context.Context, id string) ([]facade.Todo, error) {
	todos, err := c.s.handle.RemoveTodo(ctx, id)
	if err != nil {
		return nil, err
	}
	return convertTodos(todos), nil
}

// mapModelError translates the runtime's typed capacity and capability errors. Other
// errors pass through unchanged so their detail is kept.
func mapModelError(err error) error {
	var sessionErr *runtime.SessionError
	if !errors.As(err, &sessionErr) {
		return err
	}
	switch {
	case sessionErr.Kind == runtime.SessionErrorCapacity && sessionErr.Reason == runtime.SessionErrorReasonBusy:
		return fmt.Errorf("%w: %w", facade.ErrBusy, err)
	case sessionErr.Kind == runtime.SessionErrorUnsupported:
		return fmt.Errorf("%w: %w", facade.ErrUnsupported, err)
	default:
		return err
	}
}

type modelsCap struct{ s *sessionImpl }

func (c modelsCap) Available(ctx context.Context) []modelcatalog.ModelChoice {
	return c.s.handle.AvailableModels(ctx)
}

func (c modelsCap) Set(ctx context.Context, ref string) error {
	return mapModelError(c.s.handle.SetModel(ctx, ref))
}

type thinkingCap struct{ s *sessionImpl }

func (c thinkingCap) Levels(ctx context.Context) []effort.Level {
	return c.s.handle.ThinkingLevels(ctx)
}

func (c thinkingCap) Current(ctx context.Context) effort.Level {
	return c.s.handle.CurrentThinkingLevel(ctx)
}

func (c thinkingCap) Set(ctx context.Context, level effort.Level) (effort.Level, error) {
	applied, err := c.s.handle.SetThinkingLevel(ctx, level)
	return applied, mapModelError(err)
}

func (c thinkingCap) Cycle(ctx context.Context) (effort.Level, error) {
	level, err := c.s.handle.CycleThinkingLevel(ctx)
	return level, mapModelError(err)
}

type agentSwitchCap struct{ s *sessionImpl }

// SwitchAgent changes the same session in place. Busy, unknown-agent and unsupported
// failures map to the facade's typed errors so callers branch on errors.Is.
func (c agentSwitchCap) SwitchAgent(ctx context.Context, agentName string) error {
	switcher, ok := c.s.handle.(runtime.ActiveAgentSwitcher)
	if !ok {
		return fmt.Errorf("switch agent: %w", facade.ErrUnsupported)
	}
	err := switcher.SwitchActiveAgent(ctx, agentName)
	var sessionErr *runtime.SessionError
	if !errors.As(err, &sessionErr) {
		return err
	}
	switch {
	case sessionErr.Kind == runtime.SessionErrorCapacity && sessionErr.Reason == runtime.SessionErrorReasonBusy:
		return fmt.Errorf("%w: %w", facade.ErrBusy, err)
	case sessionErr.Kind == runtime.SessionErrorNotFound, sessionErr.Kind == runtime.SessionErrorInvalid:
		return fmt.Errorf("%w: %w", facade.ErrUnknownAgent, err)
	case sessionErr.Kind == runtime.SessionErrorUnsupported:
		return fmt.Errorf("%w: %w", facade.ErrUnsupported, err)
	default:
		return err
	}
}

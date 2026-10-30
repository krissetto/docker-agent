package handlefacade

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/runtime/facade"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/subagent"
)

type sessionImpl struct {
	host   *host
	handle runtime.SessionHandle

	mu     sync.Mutex
	feed   *feed
	closed bool
}

func newSession(h *host, handle runtime.SessionHandle) *sessionImpl {
	return &sessionImpl{host: h, handle: handle}
}

// currentInfo reads the handle's live metadata, so an agent switch or a model
// change is reflected on the next call rather than at adoption.
func (s *sessionImpl) currentInfo() facade.Info {
	meta := s.handle.Metadata()
	return facade.Info{
		SessionID:     s.handle.ID(),
		AgentName:     meta.AgentName,
		Model:         meta.Model,
		ThinkingLevel: string(meta.ThinkingLevel),
		Capabilities:  capabilitiesOf(s.handle, meta),
	}
}

func (s *sessionImpl) capabilities() facade.Capabilities { return s.currentInfo().Capabilities }

// capabilitiesOf gates each optional capability on the handle's facts and on
// the interfaces the handle actually implements.
func capabilitiesOf(handle runtime.SessionHandle, meta runtime.SessionMetadata) facade.Capabilities {
	facts := meta.Capabilities
	_, canWithdraw := handle.(runtime.PendingMessageCanceler)
	_, canStop := handle.(runtime.SessionTreeController)
	_, canSwitch := handle.(runtime.ActiveAgentSwitcher)
	return facade.Capabilities{
		Queue:        facts.ExecutionAvailable,
		Withdraw:     facts.PendingMessageRemoval && canWithdraw,
		Pending:      facts.ExecutionAvailable && facts.SessionEditing,
		Compact:      facts.Compaction,
		Pause:        facts.Pause,
		Observer:     true,
		SubagentTree: facts.DelegationPolicy,
		StopSubtree:  facts.StopSubtree && canStop,
		TodoEditor:   facts.TodoEditing,
		Background:   facts.ExecutionAvailable,
		Models:       facts.ModelSwitching,
		Thinking:     facts.ThinkingLevels,
		AgentSwitch:  facts.ActiveAgentSwitching && canSwitch,
	}
}

func (s *sessionImpl) Info() facade.Info { return s.currentInfo() }

func (s *sessionImpl) Status(ctx context.Context) (facade.Status, error) {
	st, err := s.handle.Status(ctx)
	if err != nil {
		return facade.Status{}, err
	}
	return mapStatus(st), nil
}

func (s *sessionImpl) Snapshot(ctx context.Context) (*session.Session, error) {
	return s.handle.Snapshot(ctx)
}

func (s *sessionImpl) Close(context.Context) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	f := s.feed
	s.mu.Unlock()
	s.host.forget(s)
	if f != nil {
		f.requestClose()
	}
	return nil
}

// ensureFeed starts the session observation on first use. It is never tied to a
// caller context after establishment.
func (s *sessionImpl) ensureFeed(ctx context.Context) (*feed, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, facade.ErrSessionClosed
	}
	if s.feed != nil {
		return s.feed, nil
	}
	f, err := startFeed(ctx, s.handle, s.handle.ID())
	if err != nil {
		return nil, fmt.Errorf("observe session: %w", err)
	}
	s.feed = f
	return f, nil
}

func (s *sessionImpl) runningFeed() *feed {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.feed
}

func (s *sessionImpl) Run(ctx context.Context, input facade.Input) (facade.Turn, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f, err := s.ensureFeed(ctx)
	if err != nil {
		return nil, err
	}
	requestID := input.RequestID
	if requestID == "" {
		if requestID, err = newRequestID(); err != nil {
			return nil, err
		}
	}
	turnID := deriveTurnID(s.handle.ID(), requestID)
	ts, err := f.register(turnID)
	if err != nil {
		return nil, err
	}
	turnInput := s.turnInput(ctx, input)
	turnInput.RequestID = requestID
	submission, err := s.handle.Submit(ctx, turnInput)
	if err != nil {
		f.unregister(ts)
		return nil, err
	}
	if submission.TurnID != turnID {
		// Admission succeeded under an identity the feed cannot see; stop it rather than run untracked.
		f.unregister(ts)
		_, _ = s.handle.Cancel(context.WithoutCancel(ctx), submission.TurnID)
		return nil, errors.New("runtime turn identity does not match the request derivation")
	}
	return &turn{sess: s, st: ts}, nil
}

func (s *sessionImpl) turnInput(ctx context.Context, input facade.Input) runtime.TurnInput {
	return runtime.TurnInput{
		RequestID:      input.RequestID,
		Content:        input.Content,
		MultiContent:   input.MultiContent,
		GenerateTitle:  input.GenerateTitle,
		TitleGenerator: s.host.titleGenerator(ctx),
	}
}

func (s *sessionImpl) Respond(ctx context.Context, response facade.Response) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if (response.Decision == nil) == (response.Elicitation == nil) {
		return fmt.Errorf("%w: exactly one of decision or elicitation is required", facade.ErrInvalidResponse)
	}
	id, pending, err := s.resolveInteraction(ctx, response.InteractionID)
	if err != nil {
		return err
	}
	request := runtime.InteractionResponse{InteractionID: id}
	switch pending.kind {
	case facade.InteractionConfirmation, facade.InteractionMaxIterations:
		if response.Decision == nil {
			return fmt.Errorf("%w: %s requires a decision", facade.ErrInvalidResponse, pending.kind)
		}
		resume, err := resumeFor(*response.Decision)
		if err != nil {
			return err
		}
		request.Kind = runtime.InteractionConfirmation
		if pending.kind == facade.InteractionMaxIterations {
			request.Kind = runtime.InteractionMaxIterations
		}
		request.Resume = resume
	case facade.InteractionElicitation:
		if response.Elicitation == nil {
			return fmt.Errorf("%w: elicitation requires a reply", facade.ErrInvalidResponse)
		}
		request.Kind = runtime.InteractionElicitation
		request.ElicitationID = pending.elicitationID
		request.Elicitation = runtime.ElicitationResult{Action: response.Elicitation.Action, Content: response.Elicitation.Content}
	default:
		return fmt.Errorf("%w: unknown interaction kind %q", facade.ErrInvalidResponse, pending.kind)
	}
	if err := s.handle.Respond(ctx, request); err != nil {
		return mapRespondError(err, id)
	}
	return nil
}

// resolveInteraction returns the outstanding interaction a response targets. The
// feed registry answers first; an observation snapshot refreshes it on a miss.
func (s *sessionImpl) resolveInteraction(ctx context.Context, id string) (string, pendingInteraction, error) {
	f := s.runningFeed()
	var known map[string]pendingInteraction
	if f != nil {
		known = f.outstandingSnapshot()
	} else {
		known = map[string]pendingInteraction{}
	}
	if id == "" {
		if len(known) == 0 {
			known = s.refreshOutstanding(ctx)
		}
		switch len(known) {
		case 0:
			return "", pendingInteraction{}, facade.ErrNotActive
		case 1:
			for only, pending := range known {
				return only, pending, nil
			}
		}
		return "", pendingInteraction{}, facade.ErrAmbiguousInteraction
	}
	if pending, ok := known[id]; ok {
		return id, pending, nil
	}
	known = s.refreshOutstanding(ctx)
	if pending, ok := known[id]; ok {
		return id, pending, nil
	}
	if f != nil && f.wasResolved(id) {
		return "", pendingInteraction{}, fmt.Errorf("%w: %s", facade.ErrStaleInteraction, id)
	}
	return "", pendingInteraction{}, fmt.Errorf("%w: %s", facade.ErrUnknownInteraction, id)
}

func (s *sessionImpl) refreshOutstanding(ctx context.Context) map[string]pendingInteraction {
	out := map[string]pendingInteraction{}
	snapshot, err := observeOnce(ctx, s.handle)
	if err != nil {
		return out
	}
	for _, interaction := range snapshot.Interactions {
		out[interaction.InteractionID] = pendingInteraction{kind: facade.InteractionKind(interaction.Kind), elicitationID: interaction.ElicitationID}
	}
	return out
}

func resumeFor(decision facade.Decision) (runtime.ResumeRequest, error) {
	switch decision.Type {
	case facade.DecisionApprove:
		return runtime.ResumeApprove(), nil
	case facade.DecisionApproveBalanced:
		return runtime.ResumeApproveBalanced(), nil
	case facade.DecisionApproveAutonomous:
		return runtime.ResumeApproveAutonomous(), nil
	case facade.DecisionApproveTool:
		if decision.ToolName == "" {
			return runtime.ResumeRequest{}, fmt.Errorf("%w: approve-tool requires a tool name", facade.ErrInvalidResponse)
		}
		return runtime.ResumeApproveTool(decision.ToolName), nil
	case facade.DecisionReject:
		return runtime.ResumeReject(decision.Reason), nil
	default:
		return runtime.ResumeRequest{}, fmt.Errorf("%w: unknown decision %q", facade.ErrInvalidResponse, decision.Type)
	}
}

func mapRespondError(err error, id string) error {
	var sessionErr *runtime.SessionError
	if errors.As(err, &sessionErr) {
		switch sessionErr.Kind {
		case runtime.SessionErrorStale:
			return fmt.Errorf("%w: %s: %w", facade.ErrStaleInteraction, id, err)
		case runtime.SessionErrorNotFound:
			return fmt.Errorf("%w: %s: %w", facade.ErrUnknownInteraction, id, err)
		case runtime.SessionErrorInvalid:
			return fmt.Errorf("%w: %w", facade.ErrInvalidResponse, err)
		}
	}
	return err
}

func (s *sessionImpl) Queue() (facade.Queue, bool) {
	if !s.capabilities().Queue {
		return nil, false
	}
	return queueCap{s}, true
}

func (s *sessionImpl) Withdraw() (facade.Withdrawer, bool) {
	if !s.capabilities().Withdraw {
		return nil, false
	}
	return withdrawCap{s}, true
}

func (s *sessionImpl) Pending() (facade.PendingInputs, bool) {
	if !s.capabilities().Pending {
		return nil, false
	}
	return pendingCap{s}, true
}

func (s *sessionImpl) Compact() (facade.Compactor, bool) {
	if !s.capabilities().Compact {
		return nil, false
	}
	return compactCap{s}, true
}

func (s *sessionImpl) Pause() (facade.Pauser, bool) {
	if !s.capabilities().Pause {
		return nil, false
	}
	return pauseCap{s}, true
}

func (s *sessionImpl) Observer() (facade.Observer, bool) {
	if !s.capabilities().Observer {
		return nil, false
	}
	return observerCap{s}, true
}

func (s *sessionImpl) SubagentTree() (facade.SubagentTreeReader, bool) {
	if !s.capabilities().SubagentTree {
		return nil, false
	}
	return treeCap{s}, true
}

func (s *sessionImpl) StopSubtree() (facade.SubtreeStopper, bool) {
	if !s.capabilities().StopSubtree {
		return nil, false
	}
	return stopCap{s}, true
}

func (s *sessionImpl) TodoEditor() (facade.TodoEditor, bool) {
	if !s.capabilities().TodoEditor {
		return nil, false
	}
	return todoCap{s}, true
}

func (s *sessionImpl) Background() (facade.Background, bool) {
	if !s.capabilities().Background {
		return nil, false
	}
	return backgroundCap{s}, true
}

// observeOnce captures the authoritative snapshot without keeping the observation.
func observeOnce(ctx context.Context, handle runtime.SessionHandle) (runtime.SessionSnapshot, error) {
	observation, err := handle.Observe(ctx, runtime.ObserveOptions{})
	if err != nil {
		return runtime.SessionSnapshot{}, err
	}
	snapshot := observation.Primary()
	if observation.Cancel != nil {
		observation.Cancel()
	}
	return snapshot, nil
}

func parseUpdated(value string) time.Time {
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return time.Time{}
	}
	return parsed
}

func convertTree(snapshot *subagent.Snapshot) facade.Tree {
	if snapshot == nil {
		return facade.Tree{}
	}
	var convert func([]subagent.NodeSnapshot) []facade.TreeNode
	convert = func(nodes []subagent.NodeSnapshot) []facade.TreeNode {
		out := make([]facade.TreeNode, 0, len(nodes))
		for _, n := range nodes {
			out = append(out, facade.TreeNode{
				ID:             string(n.Node.ID),
				Parent:         string(n.Node.Parent),
				SessionID:      n.Node.SessionID,
				Agent:          n.Node.Agent,
				Task:           n.Node.Task,
				State:          string(n.Node.State),
				NeedsAttention: n.Node.NeedsAttention,
				WaitingOn:      n.Node.WaitingOn,
				Error:          n.Node.Error,
				Children:       convert(n.Children),
			})
		}
		return out
	}
	return facade.Tree{Roots: convert(snapshot.Nodes)}
}

func convertTodos(todos []session.Todo) []facade.Todo {
	out := make([]facade.Todo, 0, len(todos))
	for _, todo := range todos {
		out = append(out, facade.Todo{ID: todo.ID, Description: todo.Description, Status: todo.Status})
	}
	return out
}

func mapInteractions(in []runtime.InteractionSnapshot) []facade.Interaction {
	out := make([]facade.Interaction, 0, len(in))
	for _, interaction := range in {
		out = append(out, facade.Interaction{InteractionID: interaction.InteractionID, Kind: facade.InteractionKind(interaction.Kind), ElicitationID: interaction.ElicitationID})
	}
	return out
}

func mapStatus(st runtime.SessionStatus) facade.Status {
	return facade.Status{
		Busy:          st.State == runtime.SessionStateQueued || st.State == runtime.SessionStateRunning || st.State == runtime.SessionStateCancelling,
		Paused:        st.Paused,
		PauseArmed:    st.PauseArmed,
		PendingCount:  st.Pending,
		CurrentTurnID: st.TurnID,
	}
}

func (s *sessionImpl) Models() (facade.Models, bool) {
	if !s.capabilities().Models {
		return nil, false
	}
	return modelsCap{s}, true
}

func (s *sessionImpl) Thinking() (facade.Thinking, bool) {
	if !s.capabilities().Thinking {
		return nil, false
	}
	return thinkingCap{s}, true
}

func (s *sessionImpl) AgentSwitch() (facade.AgentSwitcher, bool) {
	if !s.capabilities().AgentSwitch {
		return nil, false
	}
	return agentSwitchCap{s}, true
}

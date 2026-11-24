package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/docker/docker-agent/pkg/effort"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/skills"
	"github.com/docker/docker-agent/pkg/subagent"
	"github.com/docker/docker-agent/pkg/teamdef"
	"github.com/docker/docker-agent/pkg/tools"
	skillstool "github.com/docker/docker-agent/pkg/tools/builtin/skills"
)

const (
	historicalDefinitionMissing = "definition_not_recorded"
	historicalAgentUnavailable  = "agent_unavailable"
)

func historicalError(id string, operation SessionOperation) error {
	return &SessionError{Kind: SessionErrorHistorical, SessionID: id, Operation: operation}
}

type historicalSessionHandle struct {
	UnsupportedSessionHandle
	id         string
	snapshot   *session.Session
	store      session.Store
	history    session.ViewHistoryStore
	metadata   SessionMetadata
	definition *teamdef.AgentDefinition
}

func newHistoricalSessionHandle(id string, snapshot *session.Session, metadata SessionMetadata, definition *teamdef.AgentDefinition) *historicalSessionHandle {
	return &historicalSessionHandle{id: id, snapshot: snapshot.Clone(), metadata: metadata, definition: definition}
}

func (h *historicalSessionHandle) ID() string                { return h.id }
func (h *historicalSessionHandle) AgentName() string         { return h.metadata.AgentName }
func (h *historicalSessionHandle) Metadata() SessionMetadata { return h.metadata }
func (h *historicalSessionHandle) Submit(context.Context, TurnInput) (Submission, error) {
	return Submission{}, historicalError(h.id, SessionOperationSubmit)
}
func (h *historicalSessionHandle) Retry(context.Context) (Submission, error) {
	return Submission{}, historicalError(h.id, SessionOperationSubmit)
}
func (h *historicalSessionHandle) Steer(context.Context, TurnInput) (Submission, error) {
	return Submission{}, historicalError(h.id, SessionOperationSteer)
}
func (h *historicalSessionHandle) Respond(context.Context, InteractionResponse) error {
	return historicalError(h.id, SessionOperationRespond)
}
func (h *historicalSessionHandle) Cancel(context.Context, string) (CancelResult, error) {
	return CancelResult{}, historicalError(h.id, SessionOperationCancel)
}
func (h *historicalSessionHandle) AwaitTurn(ctx context.Context, turnID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if turnID != "" {
		snapshot, err := h.Snapshot(ctx)
		if err != nil {
			return err
		}
		outcome := snapshot.TurnOutcome(turnID)
		if outcome == string(TurnCompleted) || outcome == string(TurnFailed) || outcome == string(TurnCanceled) {
			return nil
		}
		for _, item := range snapshot.MessagesSnapshot() {
			if item.Message != nil && item.Message.Accepted && item.Message.TurnID == turnID && (item.Message.ReportOutcome == session.ReportOutcomeFinished || item.Message.ReportOutcome == session.ReportOutcomeFailed) {
				return nil
			}
		}
	}
	return historicalError(h.id, SessionOperationAwaitTurn)
}

func (h *historicalSessionHandle) Snapshot(ctx context.Context) (*session.Session, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	snapshot := h.snapshot.Clone()
	if h.store != nil {
		stored, err := session.ReadSessionOwnHistory(ctx, h.store, h.id)
		if err != nil {
			return nil, err
		}
		snapshot.Messages = stored.Messages
	}
	return snapshot, nil
}
func (h *historicalSessionHandle) Status(ctx context.Context) (SessionStatus, error) {
	if err := ctx.Err(); err != nil {
		return SessionStatus{}, err
	}
	return SessionStatus{SessionID: h.id, AgentName: h.metadata.AgentName, State: SessionStateSettled, LastError: h.metadata.HistoryReason}, nil
}
func (h *historicalSessionHandle) Observe(ctx context.Context, options ObserveOptions) (Observation, error) {
	if err := ctx.Err(); err != nil {
		return Observation{}, err
	}
	var snapshot SessionSnapshot
	var err error
	switch {
	case options.HistoryTail != nil:
		snapshot, err = h.tailSnapshot(ctx, *options.HistoryTail)
	case options.TreeMetadata:
		snapshot = SessionSnapshot{Projection: "metadata", Session: h.snapshot.MetadataSnapshot()}
		snapshot.Session.SetSubagentTree(h.snapshot.GetSubagentTree())
		snapshot.TranscriptPosition, err = h.historyPosition(ctx)
	default:
		snapshot.Session, err = h.Snapshot(ctx)
		if err == nil {
			snapshot.TranscriptPosition = snapshot.Session.ItemCount()
		}
	}
	if err != nil {
		return Observation{}, err
	}
	snapshot.Status = SessionStatus{SessionID: h.id, AgentName: h.metadata.AgentName, State: SessionStateSettled}
	snapshot.Epoch = "historical"
	events := make(chan SessionEvent)
	var once sync.Once
	cancel := func() { once.Do(func() { close(events) }) }
	if options.Tree {
		updates := make(chan TreeUpdate)
		close(updates)
		return Observation{Initial: []SessionSnapshot{snapshot}, TreeUpdates: updates, Events: events, Cancel: cancel}, nil
	}
	return Observation{Initial: []SessionSnapshot{snapshot}, Events: events, Cancel: cancel}, nil
}

func (h *historicalSessionHandle) Release(context.Context) error { return nil }
func (h *historicalSessionHandle) UpdateTitle(context.Context, string) error {
	return historicalError(h.id, SessionOperationUpdateTitle)
}
func (h *historicalSessionHandle) Edit(context.Context, SessionEdit) (*session.Session, error) {
	return nil, historicalError(h.id, "edit")
}
func (h *historicalSessionHandle) SetTodoStatus(context.Context, string, string) ([]session.Todo, error) {
	return nil, historicalError(h.id, SessionOperationTodos)
}
func (h *historicalSessionHandle) SetTodoDescription(context.Context, string, string, string) ([]session.Todo, error) {
	return nil, historicalError(h.id, SessionOperationTodos)
}
func (h *historicalSessionHandle) RemoveTodo(context.Context, string) ([]session.Todo, error) {
	return nil, historicalError(h.id, SessionOperationTodos)
}
func (h *historicalSessionHandle) Compact(context.Context, string, EventSink) error {
	return historicalError(h.id, SessionOperationCompact)
}
func (h *historicalSessionHandle) CompactTarget(context.Context, string, string, EventSink) error {
	return historicalError(h.id, SessionOperationCompactTarget)
}
func (h *historicalSessionHandle) ContextBreakdown(context.Context) (*ContextBreakdown, error) {
	return nil, historicalError(h.id, SessionOperationContext)
}
func (h *historicalSessionHandle) LiveSessions(context.Context) ([]LiveSession, error) {
	return nil, historicalError(h.id, SessionOperationLiveSessions)
}
func (h *historicalSessionHandle) Skills(ctx context.Context) ([]skills.Skill, error) {
	return nil, ctx.Err()
}
func (h *historicalSessionHandle) ResolveSkillCommand(ctx context.Context, _ string) (string, error) {
	return "", ctx.Err()
}
func (h *historicalSessionHandle) RunSkillFork(context.Context, skillstool.RunSkillArgs, EventSink) (*tools.ToolCallResult, error) {
	return nil, historicalError(h.id, SessionOperationRunSkill)
}
func (h *historicalSessionHandle) StartSkillFork(context.Context, string, skillstool.RunSkillArgs) error {
	return historicalError(h.id, SessionOperationRunSkill)
}
func (h *historicalSessionHandle) TogglePause(context.Context) (bool, error) {
	return false, historicalError(h.id, SessionOperationPause)
}
func (h *historicalSessionHandle) RefreshModelsCatalog(context.Context) error {
	return historicalError(h.id, SessionOperationRefreshModels)
}
func (h *historicalSessionHandle) SetStarred(context.Context, bool) error {
	return historicalError(h.id, SessionOperationSetStarred)
}
func (h *historicalSessionHandle) RemoveAttachment(context.Context, string) error {
	return historicalError(h.id, SessionOperationRemoveAttachment)
}
func (h *historicalSessionHandle) AvailableModels(context.Context) []ModelChoice { return nil }
func (h *historicalSessionHandle) SetModel(context.Context, string) error {
	return historicalError(h.id, SessionOperationSetModel)
}
func (h *historicalSessionHandle) CycleThinkingLevel(context.Context) (effort.Level, error) {
	return "", historicalError(h.id, SessionOperationThinkingLevel)
}
func (h *historicalSessionHandle) SetThinkingLevel(context.Context, effort.Level) (effort.Level, error) {
	return "", historicalError(h.id, SessionOperationThinkingLevel)
}
func (h *historicalSessionHandle) ThinkingLevels(context.Context) []effort.Level     { return nil }
func (h *historicalSessionHandle) CurrentThinkingLevel(context.Context) effort.Level { return "" }
func (h *historicalSessionHandle) EmitPinnedAgentInfo(ctx context.Context, sink EventSink) {
	if sink == nil || h.definition == nil {
		return
	}
	d := h.definition
	sink.Emit(AgentInfo(h.metadata.AgentName, "", d.Description, d.WelcomeMessage))
	sink.Emit(TeamInfo([]AgentDetails{{Name: d.Name, Model: d.Model, ModelName: d.Model, ModelID: d.Model, ThinkingMode: "unknown", ThinkingLevel: "unknown"}}, d.Name))
}

func (h *historicalSessionHandle) SessionAgentInfo(ctx context.Context) (SessionAgentInfo, error) {
	if err := ctx.Err(); err != nil {
		return SessionAgentInfo{}, err
	}
	out := SessionAgentInfo{}
	if h.definition != nil {
		d := h.definition
		event, _ := AgentInfo(d.Name, "", d.Description, d.WelcomeMessage).(*AgentInfoEvent)
		out.Agent = event
		out.Team = TeamInfo([]AgentDetails{{Name: d.Name, Model: d.Model, ModelName: d.Model, ModelID: d.Model, ThinkingMode: "unknown", ThinkingLevel: "unknown"}}, d.Name).(*TeamInfoEvent)
	}
	return out, nil
}

func (h *historicalSessionHandle) SessionAgentConfig(ctx context.Context, name string) (SessionAgentConfig, error) {
	if err := ctx.Err(); err != nil {
		return SessionAgentConfig{}, err
	}
	if h.definition == nil {
		if name != h.metadata.AgentName {
			return SessionAgentConfig{}, &SessionError{Kind: SessionErrorNotFound, SessionID: h.id, Operation: "agent_config"}
		}
		return SessionAgentConfig{SessionID: h.id, Source: h.snapshot.AttributesSnapshot()["docker-agent.actor.source"], AgentName: name, Historical: true, ExecutionAvailable: false, HistoryReason: h.metadata.HistoryReason}, nil
	}
	if name != h.definition.AgentID && name != h.definition.AgentKey && name != h.definition.Name && name != h.metadata.AgentName {
		return SessionAgentConfig{}, &SessionError{Kind: SessionErrorNotFound, SessionID: h.id, Operation: "agent_config"}
	}
	info := AgentConfigInfo{IsCurrent: name == h.metadata.AgentName}
	out := SessionAgentConfig{SessionID: h.id, Source: h.snapshot.AttributesSnapshot()["docker-agent.actor.source"], AgentName: name, Info: info, Historical: true, ExecutionAvailable: false, HistoryReason: h.metadata.HistoryReason}
	if h.definition != nil {
		d := h.definition
		info.MaxIterations = d.MaxIterations
		if d.ReadOnly {
			info.Options = []string{"readonly"}
		}
		out.Info, out.AgentID, out.AgentKey, out.DefinitionKnown, out.Definition = info, d.AgentID, d.AgentKey, true, d
	}
	return out, nil
}

var _ SessionHandle = (*historicalSessionHandle)(nil)
var _ SessionAgentConfigReader = (*historicalSessionHandle)(nil)
var _ SessionAgentInfoProvider = (*historicalSessionHandle)(nil)

func (v *localSessionRuntimeView) historicalSessionView(ctx context.Context, selectedMeta *session.Session) (*preparedSessionView, error) {
	if selectedMeta == nil {
		return nil, session.ErrNotFound
	}
	store := v.runtime.sessionStore
	selected, err := session.ReadSessionOwnHistory(ctx, store, selectedMeta.ID)
	if err != nil {
		return nil, err
	}
	root := selectedMeta
	seen := map[string]bool{root.ID: true}
	for depth := 0; root.ParentID != ""; depth++ {
		if depth >= 32 || seen[root.ParentID] {
			return nil, &SessionError{Kind: SessionErrorInvalid, SessionID: selected.ID, Operation: "restore_ancestry"}
		}
		parent, err := session.ReadSessionMetadata(ctx, store, root.ParentID)
		if err != nil {
			return nil, err
		}
		if selectedMeta.AttributesSnapshot()["docker-agent.actor.source"] != "" && parent.AttributesSnapshot()["docker-agent.actor.source"] != selectedMeta.AttributesSnapshot()["docker-agent.actor.source"] {
			return nil, &SessionError{Kind: SessionErrorWrongSession, SessionID: selected.ID, Operation: "restore_source"}
		}
		seen[parent.ID] = true
		root = parent
	}
	attrs := selectedMeta.AttributesSnapshot()
	recorded := attrs[SessionAgentAttribute]
	if recorded == "" {
		recorded = selectedMeta.AgentName
	}
	displayName := recorded
	var attach *SubagentAttachInfo
	if selected.ParentID != "" {
		reader, ok := store.(session.ChildRecordReader)
		if !ok {
			return nil, &SessionError{Kind: SessionErrorNotFound, SessionID: selected.ID, Operation: "restore_tree_membership"}
		}
		record, err := reader.LoadChild(ctx, selected.ID)
		if err != nil {
			return nil, err
		}
		if record.RootSessionID != root.ID || record.ParentSessionID != selected.ParentID || record.Node.SessionID != selected.ID || recorded != "" && record.Node.Agent != recorded {
			return nil, &SessionError{Kind: SessionErrorInvalid, SessionID: selected.ID, Operation: "restore_tree_membership"}
		}
		if displayName == "" {
			displayName = record.Node.Agent
		}
		parent, err := session.ReadSessionMetadata(ctx, store, selected.ParentID)
		if err != nil {
			return nil, err
		}
		attach = &SubagentAttachInfo{NodeID: record.Node.ID, Name: record.Node.DisplayName(), Agent: displayName, Session: selected.Clone(), ParentSessionID: selected.ParentID, ParentAgent: parent.AgentName}
	}
	if displayName != "" {
		selected.AgentName = displayName
	}
	reason := historicalDefinitionMissing
	var definition *teamdef.AgentDefinition
	if revisions, ok := store.(session.TeamRevisionStore); ok {
		if revision, revErr := revisions.LatestTeamRevision(ctx, selected.ID); revErr == nil {
			if entry, entryErr := revisions.TeamAgentDefinition(ctx, revision.AgentDigest); entryErr == nil {
				var saved teamdef.AgentDefinition
				if jsonErr := json.Unmarshal(entry.Payload, &saved); jsonErr == nil && saved.TeamID == revision.TeamID && saved.AgentID == revision.AgentID && saved.AgentKey == revision.AgentKey {
					definition = &saved
					reason = historicalAgentUnavailable
					if len(saved.Unavailable) > 0 {
						reason = "partial_definition: " + strings.Join(saved.Unavailable, ",")
					}
				}
			}
		}
	}
	selected.SetSubagentTree(nil)
	if recorded == "" {
		reason = historicalDefinitionMissing
	}
	info := PreparedSessionViewInfo{ActiveAgentName: displayName, SessionID: selected.ID, RootSessionID: root.ID, RootWorkingDir: root.WorkingDir, RootBinding: SessionBinding{AgentName: root.AttributesSnapshot()[SessionAgentAttribute]}, Session: selected, WorkingDir: selected.WorkingDir, Binding: SessionBinding{AgentName: recorded, ParentSessionID: selected.ParentID, Durability: v.runtime.sessionDurability()}, Attach: attach, Historical: true, HistoryReason: reason}
	p := &preparedSessionView{r: v.runtime, info: info, root: root, historical: true, existing: map[string]*sessionDriver{}}
	p.historicalHandle = newHistoricalSessionHandle(selected.ID, selected, SessionMetadata{SessionID: selected.ID, AgentName: displayName, Historical: true, ExecutionAvailable: false, HistoryReason: reason, Capabilities: SessionCapabilities{Historical: true, ExecutionAvailable: false}}, definition)
	return p, nil
}

func (h *historicalSessionHandle) TreeSnapshot() subagent.Snapshot {
	return subagent.Snapshot{Version: subagent.SnapshotVersion, Root: subagent.SessionRootID(h.id)}
}

func (h *historicalSessionHandle) String() string { return fmt.Sprintf("historical session %s", h.id) }

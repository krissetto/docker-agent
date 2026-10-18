package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/docker/docker-agent/pkg/api"
	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/config/types"
	"github.com/docker/docker-agent/pkg/effort"
	"github.com/docker/docker-agent/pkg/hooks/builtins"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/skills"
	"github.com/docker/docker-agent/pkg/subagent"
	"github.com/docker/docker-agent/pkg/teamdef"
	"github.com/docker/docker-agent/pkg/tools"
	skillstool "github.com/docker/docker-agent/pkg/tools/builtin/skills"
	"github.com/docker/docker-agent/pkg/tools/lifecycle"
)

const (
	sessionID = "session-demo-001"
	turnID    = "turn-demo-001"
	epoch     = "fixture-epoch-001"
)

var (
	stamp     = time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC)
	stampText = stamp.Format(time.RFC3339)
	outDir    = "apple/Tests/AgentKitTests/Fixtures"
)

type streamMessage = api.SessionStreamMessage[runtime.SessionState, runtime.InteractionKind, any]
type snapshot = api.SessionSnapshot[runtime.SessionState, runtime.InteractionKind, any]
type envelope = api.SessionEnvelope[any]

// contractExample wraps real request/response DTOs with their HTTP method,
// route, and status for test-readable synthetic examples. The wrapper is
// fixture metadata, not an API response envelope.
type contractExample struct {
	Name     string `json:"name"`
	Method   string `json:"method"`
	Path     string `json:"path"`
	Bodyless bool   `json:"bodyless,omitempty"`
	Status   int    `json:"status"`
	Request  any    `json:"request,omitempty"`
	Response any    `json:"response,omitempty"`
}

type branchProofCaseFixture struct {
	Name                    string                     `json:"name"`
	SnapshotRaw             string                     `json:"snapshot_raw,omitempty"`
	BranchProofAvailable    bool                       `json:"branch_proof_available_from_snapshot"`
	BranchUnavailableReason string                     `json:"branch_unavailable_reason,omitempty"`
	SnapshotRawSHA256       string                     `json:"snapshot_raw_sha256"`
	SnapshotMessagesRaw     string                     `json:"snapshot_messages_raw"`
	SnapshotMessagesNull    bool                       `json:"snapshot_messages_null"`
	ProofMessagesEmpty      bool                       `json:"proof_messages_empty_array"`
	ProofMessagesRaw        string                     `json:"proof_messages_raw"`
	ExpectedSnapshot        string                     `json:"expected_snapshot"`
	Requests                []api.SessionBranchRequest `json:"branch_requests"`
	ChunkFrames             []streamMessage            `json:"snapshot_chunk_frames,omitempty"`
}

type toolToggleState struct {
	Name  string          `json:"name"`
	State json.RawMessage `json:"session_state"`
}

type toolToggleSemanticsFixture struct {
	Endpoint string          `json:"endpoint"`
	Method   string          `json:"method"`
	Bodyless bool            `json:"bodyless"`
	Response json.RawMessage `json:"response_json"`
	States   json.RawMessage `json:"session_states"`
}

func main() {
	if err := run(); err != nil {
		panic(err)
	}
}
func run() error {
	if err := os.MkdirAll(outDir, 0755); err != nil {
		return err
	}

	sess := makeSession()
	serverInfo := api.ServerInfo{
		Version: 1, SessionAPIVersion: api.SessionAPIVersion,
		InstanceID: "fixture-instance-001", Ready: true,
		WorkspaceRoot: "/workspace/demo", Source: "demo-team",
		ConfigFingerprint: "sha256:synthetic-config",
		Capabilities:      []string{"session_v2", "session_tools", "session_permissions", "mcp_prompts", "todo_editing", "session_branching", "session_explicit_ids"},
	}
	if err := writeJSON("server_identity.json", serverInfo); err != nil {
		return err
	}
	agents := []api.Agent{{
		Name: "workspace-source.yaml", RootAgentName: "planner",
		Description: "The actual configured root", Multi: true,
		Commands: []string{"review"},
	}}
	if err := writeJSON("agents.json", agents); err != nil {
		return err
	}

	mediaParts := api.SessionInputRequest{
		Mode: "submit", Content: "Please inspect the attached notes and chart.",
		RequestID: "fixture-attachment-request",
		MultiContent: []chat.MessagePart{
			{Type: chat.MessagePartTypeDocument, Document: &chat.Document{
				Name: "notes.txt", MimeType: "text/plain", Size: 17,
				Source: chat.DocumentSource{InlineText: "sample attachment\n"},
			}},
			{Type: chat.MessagePartTypeDocument, Document: &chat.Document{
				Name: "chart.png", MimeType: "image/png", Size: 3,
				Source: chat.DocumentSource{InlineData: []byte{0x89, 0x50, 0x4e}},
			}},
		},
	}
	if err := writeJSON("session_media_parts.json", mediaParts); err != nil {
		return err
	}
	mediaRef := runtime.GeneratedFileRef{OwnerSessionID: sessionID, Root: chat.ArtifactRootWorkspace, Path: "generated/chart.png"}
	if err := writeJSON("generated_media_ref.json", mediaRef); err != nil {
		return err
	}
	generatedMediaMessage := session.NewAgentMessage("root", &chat.Message{
		Role: chat.MessageRoleAssistant, Content: "Chart generated.", CreatedAt: stampText,
		MultiContent: []chat.MessagePart{{Type: chat.MessagePartTypeDocument, Document: &chat.Document{
			Name: "chart.png", MimeType: "image/png", Size: 3,
			Source: chat.DocumentSource{ArtifactOwnerSessionID: sessionID, ArtifactRoot: chat.ArtifactRootWorkspace, ArtifactPath: "generated/chart.png", InlineData: []byte{0x89, 0x50, 0x4e}},
		}}},
	})
	generatedAdded := runtime.MessageAddedAt(sessionID, generatedMediaMessage, "root", 2).(*runtime.MessageAddedEvent)
	generatedAdded.AgentContext.Timestamp = stamp
	generatedMediaEvent := api.SessionEnvelope[any]{Version: api.SessionAPIVersion, SessionID: sessionID, Epoch: epoch, Sequence: 17, TranscriptPosition: 2, Event: generatedAdded}
	if err := writeJSON("generated_media_event.json", generatedMediaEvent); err != nil {
		return err
	}

	toolDefinition := tools.Tool{
		Name: "read_file", Category: "filesystem", Description: "Read a workspace file",
		Parameters:  map[string]any{"type": "object", "properties": map[string]any{"path": map[string]any{"type": "string"}}, "required": []string{"path"}},
		Annotations: tools.ToolAnnotations{ReadOnlyHint: true},
	}
	toolInfo := runtime.SessionToolsInfo{
		Tools:    []tools.Tool{toolDefinition},
		Statuses: []api.SessionToolsetStatus{{Name: "filesystem", Kind: "filesystem", Description: "Workspace files", State: lifecycle.StateReady, Restartable: false}},
	}
	if err := writeJSON("session_tools.json", toolInfo); err != nil {
		return err
	}
	permissions := runtime.SessionPermissionsInfo{
		Policy:  session.SafetyPolicyBalanced,
		Source:  &runtime.PermissionsInfo{Allow: []string{"read_file"}, Ask: []string{"write_file"}, Deny: []string{"shell"}},
		Session: &session.PermissionsConfig{Allow: []string{"docs/*"}, Ask: []string{"write_file"}, Deny: []string{"shell"}},
	}
	if err := writeJSON("session_permissions.json", permissions); err != nil {
		return err
	}

	agentInfo := runtime.SessionAgentInfo{
		Agent: &runtime.AgentInfoEvent{AgentContext: runtime.AgentContext{AgentName: "planner", Timestamp: stamp}, Type: "agent_info", AgentName: "planner", Model: "demo/model", Description: "Planning agent", WelcomeMessage: "Ready"},
		Team: &runtime.TeamInfoEvent{AgentContext: runtime.AgentContext{AgentName: "planner", Timestamp: stamp}, Type: "team_info", CurrentAgent: "planner", AvailableAgents: []runtime.AgentDetails{
			{Name: "planner", Description: "Planning agent", Model: "demo/model", ModelID: "demo/model", ModelName: "Demo Model", ThinkingMode: "effort", ThinkingLevel: "medium", ThinkingLevels: []string{"low", "medium", "high"}, CanCycleThinking: true},
			{Name: "worker", Description: "Implementation agent", Model: "demo/worker", ModelID: "demo/worker", ModelName: "Demo Worker"},
		}},
		Commands: types.Commands{"review": {Description: "Review changes", Instruction: "Inspect the provided changes."}},
	}
	if err := writeJSON("agent_info.json", agentInfo); err != nil {
		return err
	}
	agentConfig := runtime.SessionAgentConfig{
		SessionID: sessionID, Source: "demo-team", AgentName: "planner",
		Info:    runtime.AgentConfigInfo{SubAgents: []string{"worker"}, Handoffs: []string{"worker"}, Fallbacks: []string{"demo/fallback"}, Skills: []string{"review"}, MaxIterations: 8, NumHistoryItems: 6, MaxConsecutiveToolCalls: 5, Options: []string{"add-date"}, Toolsets: []runtime.ToolsetDetail{{Name: "filesystem", Kind: "filesystem", State: runtime.ToolsetStarted, Tools: []string{"read_file", "list_files"}}}, IsCurrent: true},
		AgentID: "planner-id", AgentKey: "planner", DefinitionKnown: true,
		Definition: &teamdef.AgentDefinition{FormatVersion: 1, TeamID: "demo-team", AgentID: "planner-id", AgentKey: "planner", Name: "Planner", Description: "Planning agent", WelcomeMessage: "Ready", Instruction: "Provide a plan.", Model: "demo/model", Safety: "balanced", ReadOnly: true, MaxIterations: 8, Toolsets: []teamdef.ToolsetDefinition{{Type: "filesystem", Name: "filesystem", Tools: []string{"read_file"}, ReadOnly: true}}, SubAgents: []string{"worker"}, Handoffs: []string{"worker"}},
	}
	if err := writeJSON("agent_config.json", agentConfig); err != nil {
		return err
	}

	prompts := map[string]tools.PromptInfo{
		"0/review_summary": {Name: "review_summary", Description: "Summarize a code review", Arguments: []tools.PromptArgument{{Name: "diff", Description: "Patch or diff text", Required: true}, {Name: "focus", Description: "Optional review focus", Required: false}}},
	}
	if err := writeJSON("mcp_prompts.json", prompts); err != nil {
		return err
	}
	promptRequest := api.SessionPromptRequest{Name: "0/review_summary", Args: map[string]string{"diff": "diff --git a/example.txt b/example.txt", "focus": "correctness"}}
	if err := writeJSON("mcp_prompt_execute_request.json", promptRequest); err != nil {
		return err
	}
	if err := writeJSON("mcp_prompt_execute_response.json", api.SessionPromptResult{Text: "Review notes: inspect the changed condition."}); err != nil {
		return err
	}

	forkSkills := []skills.Skill{
		{Name: "review-plan", Description: "Produce a review plan", FilePath: "fixtures/skills/review-plan/SKILL.md", BaseDir: "fixtures/skills/review-plan", Files: []string{"SKILL.md"}, Local: true, Context: "fork", Model: "demo/model", AllowedTools: []string{"read_file"}},
	}
	if err := writeJSON("fork_skills.json", forkSkills); err != nil {
		return err
	}
	skillRequest := struct {
		skillstool.RunSkillArgs
		OperationID string `json:"operation_id"`
	}{RunSkillArgs: skillstool.RunSkillArgs{Name: "review-plan", Task: "Draft a short checklist for the sample diff."}, OperationID: "skill-op-demo-001"}
	if err := writeJSON("fork_skill_run_request.json", skillRequest); err != nil {
		return err
	}
	skillAccepted := struct {
		SessionID   string `json:"session_id"`
		OperationID string `json:"operation_id"`
	}{SessionID: sessionID, OperationID: "skill-op-demo-001"}
	if err := writeJSON("fork_skill_run_response.json", skillAccepted); err != nil {
		return err
	}
	skillEvent := &runtime.SkillOperationEvent{AgentContext: runtime.AgentContext{AgentName: "planner", Timestamp: stamp}, Type: "skill_operation", SessionID: sessionID, OperationID: "skill-op-demo-001", Skill: "review-plan", Status: "accepted"}
	if err := writeJSON("fork_skill_operation_event.json", skillEvent); err != nil {
		return err
	}

	contextBreakdown := runtime.ContextBreakdown{
		SystemPrompt:      runtime.ContextCategory{Tokens: 120, Items: 2},
		ToolDefinitions:   runtime.ContextCategory{Tokens: 240, Items: 3},
		PromptFiles:       runtime.ContextCategory{Tokens: 80, Items: 1},
		Messages:          runtime.ContextCategory{Tokens: 560, Items: 4},
		ToolResults:       runtime.ContextCategory{Tokens: 110, Items: 1},
		CompactionSummary: runtime.ContextCategory{Tokens: 0, Items: 0},
		PromptFileItems:   []runtime.ContextFile{{Path: "/workspace/demo/AGENTS.md", Tokens: 80}},
		AttachedFiles:     []runtime.ContextFile{{Path: "/workspace/demo/notes.txt", Tokens: 17}},
		ContextLimit:      8192, Model: "demo/model",
	}
	if err := writeJSON("context_breakdown.json", contextBreakdown); err != nil {
		return err
	}

	snapshotInfos := []builtins.SnapshotInfo{{ID: 1, Files: 2}, {ID: 2, Files: 3}}
	workspaceProof := workspaceSnapshotProof(sessionID, snapshotInfos)
	workspaceSnapshots := api.WorkspaceSnapshots{SessionID: sessionID, Enabled: true, Files: []int{2, 3}, Proof: workspaceProof}
	if err := writeJSON("workspace_snapshots.json", workspaceSnapshots); err != nil {
		return err
	}
	if err := writeJSON("workspace_snapshot_undo_request.json", api.WorkspaceSnapshotRequest{ExpectedProof: workspaceProof}); err != nil {
		return err
	}
	if err := writeJSON("workspace_snapshot_reset_request.json", api.WorkspaceSnapshotRequest{ExpectedProof: workspaceProof, Keep: 1}); err != nil {
		return err
	}
	if err := writeJSON("workspace_snapshot_result.json", api.WorkspaceSnapshotResult{SessionID: sessionID, RestoredFiles: 3, Restored: true}); err != nil {
		return err
	}

	if err := writeJSON("batch_delete_request.json", api.BatchDeleteSessionsRequest{SessionIDs: []string{sessionID, "session-missing-002", "session-demo-child"}}); err != nil {
		return err
	}
	if err := writeJSON("batch_delete_response.json", api.BatchDeleteSessionsResponse{DeletedCount: 2, FailedCount: 1, FailedIDs: []string{"session-missing-002"}}); err != nil {
		return err
	}
	if err := writeJSON("batch_export_request.json", api.BatchExportSessionsRequest{SessionIDs: []string{sessionID, "session-missing-002"}, Format: "zip"}); err != nil {
		return err
	}
	exportedSession := map[string]any{"id": sess.ID, "title": sess.TitleSnapshot(), "created_at": sess.CreatedAt, "messages": sess.GetAllMessages(), "input_tokens": int64(120), "output_tokens": int64(48), "working_dir": sess.WorkingDir, "tools_approved": sess.ToolsApproved}
	batchExport := map[string]any{"export_format": "json", "timestamp": stampText, "sessions": []map[string]any{exportedSession}, "session_count": 1}
	if err := writeJSON("batch_export_response.json", batchExport); err != nil {
		return err
	}
	recovery := map[string]any{"id": sess.ID, "title": sess.TitleSnapshot(), "created_at": sess.CreatedAt, "messages": sess.GetAllMessages(), "input_tokens": int64(120), "output_tokens": int64(48), "working_dir": sess.WorkingDir, "tools_approved": sess.ToolsApproved, "permissions": sess.ClonePermissions(), "errors": []session.Error{{Message: "Synthetic model error", Code: runtime.ErrorCodeModelError, AgentName: "root", CreatedAt: stampText}}}
	if err := writeJSON("recovery_response.json", recovery); err != nil {
		return err
	}

	binding := runtime.SessionBinding{AgentName: "root", Model: "demo/model", Durability: subagent.DurabilityDurable}
	info := runtime.PreparedSessionViewInfo{
		ActiveAgentName: "root", SessionID: sessionID, RootSessionID: sessionID,
		RootWorkingDir: "/workspace/demo", RootBinding: binding, Session: sess,
		Binding: binding, WorkingDir: "/workspace/demo",
	}
	prepare := struct {
		Version int                             `json:"version"`
		View    string                          `json:"view"`
		Info    runtime.PreparedSessionViewInfo `json:"info"`
	}{api.SessionAPIVersion, "prepare-info", info}
	if err := writeJSON("prepare_info.json", prepare); err != nil {
		return err
	}

	metadata := api.SessionMetadata{
		SessionID: sessionID, AgentName: "root", Model: "demo/model",
		ThinkingLevels: []effort.Level{"low", "medium", "high"}, ThinkingLevel: "medium",
		Capabilities: api.SessionCapabilities{
			ExecutionAvailable: true, Snapshots: true, StopSubtree: true,
			ToolInspection: true, PermissionsInspection: true, TodoEditing: true,
			Branching: true, SessionEditing: true, ModelSwitching: true,
			Compaction: true, ThinkingLevels: true, LiveSessions: true,
			Durability: "durable", AvailableModels: []string{"demo/model"},
		},
	}
	status := api.SessionStatus[runtime.SessionState]{SessionID: sessionID, AgentName: "root", State: runtime.SessionStateRunning, Pending: 0, TurnID: turnID}
	opened := api.SessionOpened[runtime.PreparedSessionViewInfo, runtime.SessionState]{Version: api.SessionAPIVersion, View: "open-view", Info: info, Metadata: metadata, Status: status}
	if err := writeJSON("open_view.json", opened); err != nil {
		return err
	}

	created := stampText
	catalog := api.SessionCatalog[runtime.SessionState]{
		Version: api.SessionAPIVersion,
		Sources: []api.SessionCatalogSource{{Name: "demo-team", CanCreate: true}},
		Sessions: []api.SessionResource[runtime.SessionState]{{
			SessionID: sessionID, Source: "demo-team", AgentName: "root", Title: "Fixture conversation",
			NumMessages: 2, Cost: 0.0125, CreatedAt: created, ToolsApproved: false,
			SafetyPolicy: session.SafetyPolicyBalanced, InputTokens: 120, OutputTokens: 48,
			WorkingDir: "/workspace/demo", Starred: true, StateKnown: true,
			State: runtime.SessionStateRunning, Activity: "assistant", UpdatedAt: created,
			Pending: intPtr(1), Interactions: intPtr(1), Loaded: true, Attachable: true, Loadable: true,
		}},
	}
	if err := writeJSON("catalog.json", catalog); err != nil {
		return err
	}

	// These examples carry actual Go transport/runtime DTOs. The outer
	// method/path/status fields are fixture metadata, not server JSON wrappers.
	forkHistory := session.New(session.WithID("session-demo-fork-source"), session.WithClock(func() time.Time { return stamp }), session.WithTitle("Fixture conversation"), session.WithMessages([]session.Item{
		session.NewMessageItem(session.UserMessageAt(stamp, "First question.")),
		session.NewMessageItem(session.NewAgentMessage("root", &chat.Message{Role: chat.MessageRoleAssistant, Content: "First answer.", CreatedAt: stampText})),
		session.NewMessageItem(session.UserMessageAt(stamp, "Second question to prefill.")),
	}))
	forkResponse := api.SessionResponse{
		ID: "session-demo-fork-002", Title: "Fixture conversation (fork 1)",
		Messages: forkHistory.GetAllMessages()[:2], CreatedAt: stamp,
		SafetyPolicy: session.SafetyPolicyBalanced, InputTokens: 120, OutputTokens: 48,
		WorkingDir: "/workspace/demo", Permissions: &session.PermissionsConfig{Allow: []string{"read_file"}},
	}
	editMessage := &session.Message{
		ID: 4242, AgentName: "", Message: chat.Message{Role: chat.MessageRoleUser, Content: "Review the revised sample changes.", CreatedAt: stampText},
		Accepted: true, TurnID: "turn-demo-edit-001", InputMode: "submit", InputOrigin: session.InputOriginUser,
	}
	attachmentRemoval := struct {
		Path string `json:"path"`
	}{Path: "/workspace/demo/notes.txt"}
	delegationBody := struct {
		Enabled bool `json:"enabled"`
	}{Enabled: false}
	advancedExamples := []contractExample{
		{Name: "pause_toggle", Method: "POST", Path: "/api/v2/sessions/session-demo-001/pause", Bodyless: true, Status: 200, Response: map[string]bool{"paused": true}},
		{Name: "stop_subtree", Method: "POST", Path: "/api/v2/sessions/session-demo-001/stop-subtree", Bodyless: true, Status: 204},
		{Name: "wait_exact_turn", Method: "POST", Path: "/api/v2/sessions/session-demo-001/turns/turn-demo-001/wait", Bodyless: true, Status: 204},
		{Name: "wait_workflow", Method: "POST", Path: "/api/v2/sessions/session-demo-001/workflow/wait", Bodyless: true, Status: 200, Response: runtime.WorkflowSettlement{Epoch: epoch, Cursor: 19}},
		{Name: "fork_before_user_message", Method: "POST", Path: "/api/v2/sessions/session-demo-fork-source/fork", Status: 200, Request: api.ForkSessionRequest{UserMessageIndex: 1}, Response: forkResponse},
		{Name: "replace_message", Method: "PATCH", Path: "/api/v2/sessions/session-demo-001/messages/4242", Status: 200, Request: api.UpdateMessageRequest{Message: editMessage}, Response: map[string]string{"status": "updated"}},
		{Name: "get_delegation_policy", Method: "GET", Path: "/api/v2/sessions/session-demo-001/delegation-policy", Bodyless: true, Status: 200, Response: map[string]bool{"enabled": true}},
		{Name: "patch_delegation_policy", Method: "PATCH", Path: "/api/v2/sessions/session-demo-001/delegation-policy", Status: 200, Request: delegationBody, Response: map[string]bool{"enabled": false}},
		{Name: "register_source", Method: "POST", Path: api.SourceRegistrationPath, Status: 200, Request: api.SourceRegistrationRequest{Path: "teams/demo.yaml", WorkingDir: "/workspace/demo"}, Response: api.SourceRegistration{SourceID: "workspace:teams/demo.yaml", Revision: "sha256:fixture-revision-001"}},
		{Name: "remove_attachment", Method: "DELETE", Path: "/api/v2/sessions/session-demo-001/attachments", Status: 204, Request: attachmentRemoval},
	}
	if err := writeJSON("advanced_session_contract_examples.json", advancedExamples); err != nil {
		return err
	}
	childCatalog := api.SessionSummaryCatalog[runtime.SessionSummaryEntry]{
		Version: api.SessionAPIVersion, View: "summary",
		Sessions: []runtime.SessionSummaryEntry{
			{SessionID: sessionID, Source: "demo-team", AgentName: "root", Title: "Fixture conversation", NumMessages: 2, CreatedAt: stamp, UpdatedAt: created, Loaded: true, Loadable: true},
			{SessionID: "session-demo-child", ParentID: sessionID, Source: "demo-team", AgentName: "worker", Title: "Fixture child", RequiresConfirmation: true, NumMessages: 1, CreatedAt: stamp.Add(time.Minute), UpdatedAt: stampText, Loaded: false, Loadable: true},
		},
	}
	if err := writeJSON("child_catalog_candidates.json", childCatalog); err != nil {
		return err
	}
	historicalSession := session.New(session.WithID("session-demo-history"), session.WithParentID(sessionID), session.WithClock(func() time.Time { return stamp }), session.WithTitle("Historical transcript"), session.WithMessages([]session.Item{session.NewMessageItem(session.UserMessageAt(stamp, "A historical question."))}))
	historicalBinding := runtime.SessionBinding{AgentName: "worker", ParentSessionID: sessionID, Durability: subagent.DurabilityDurable}
	historicalInfo := runtime.PreparedSessionViewInfo{ActiveAgentName: "worker", SessionID: historicalSession.ID, RootSessionID: sessionID, RootWorkingDir: "/workspace/demo", RootBinding: runtime.SessionBinding{AgentName: "root", Durability: subagent.DurabilityDurable}, Session: historicalSession, Binding: historicalBinding, WorkingDir: "/workspace/demo", Historical: true, HistoryReason: "recorded child history; execution binding unavailable"}
	historicalOpen := api.SessionOpened[runtime.PreparedSessionViewInfo, runtime.SessionState]{
		Version: api.SessionAPIVersion, View: "open-view", Info: historicalInfo,
		Metadata: api.SessionMetadata{SessionID: historicalSession.ID, AgentName: "worker", Historical: true, ExecutionAvailable: false, HistoryReason: historicalInfo.HistoryReason, Capabilities: api.SessionCapabilities{Historical: true, ExecutionAvailable: false}},
		Status:   api.SessionStatus[runtime.SessionState]{SessionID: historicalSession.ID, AgentName: "worker", State: runtime.SessionStateSettled},
	}
	if err := writeJSON("historical_open_view.json", historicalOpen); err != nil {
		return err
	}
	pauseStatus := api.SessionStatus[runtime.SessionState]{SessionID: sessionID, AgentName: "root", State: runtime.SessionStateRunning, Pending: 1, TurnID: turnID, PauseArmed: true, PauseGeneration: 7}
	if err := writeJSON("pause_status.json", pauseStatus); err != nil {
		return err
	}
	if err := writeBranchProofCases(); err != nil {
		return err
	}
	if err := writeToolToggleSemantics(); err != nil {
		return err
	}

	toolCall, tool := fixtureTool()
	approval := &runtime.ToolCallConfirmationEvent{
		AgentContext: agentContext(), Type: "tool_call_confirmation", ToolCall: toolCall,
		ToolDefinition: tool, Metadata: map[string]string{"risk": "low"},
		SessionID: sessionID, RequestID: "interaction-001",
	}
	interaction := api.SessionInteraction[runtime.InteractionKind, any]{SessionID: sessionID, InteractionID: "interaction-001", Kind: runtime.InteractionConfirmation, Event: approval}
	snap := snapshot{
		Session: sess, Status: status, Interactions: []api.SessionInteraction[runtime.InteractionKind, any]{},
		PendingInputs: []api.SessionPendingInput{}, Epoch: epoch, Cursor: 0, TranscriptPosition: 2,
	}
	_ = interaction // The initial stream baseline has no outstanding prompt; the approval is in its ordered tail.
	if err := writeJSON("snapshot.json", snap); err != nil {
		return err
	}

	if err := writeEventStream(snap, toolCall, tool, approval); err != nil {
		return err
	}
	if err := writeChunkedSnapshot(); err != nil {
		return err
	}
	return nil
}

func writeBranchProofCases() error {
	childID := "session-proof-child"
	child := session.New(
		session.WithID(childID), session.WithClock(func() time.Time { return stamp }),
		session.WithParentID("session-proof-rich"), session.WithTitle("Nested <child> & notes"),
		session.WithMessages([]session.Item{
			session.NewMessageItem(session.UserMessageAt(stamp, "nested \"user\" / Ω  ")),
			{Summary: "nested summary < &"},
		}),
	)
	rich := session.New(
		session.WithID("session-proof-rich"), session.WithClock(func() time.Time { return stamp }),
		session.WithTitle("Proof <snapshot> & escaping"),
		session.WithMessages([]session.Item{
			session.NewMessageItem(session.SystemMessageAt(stamp, "system content is still part of the raw item proof")),
			session.NewMessageItem(session.UserMessageAt(stamp, "text <tag> & \"quote\" / Ω  ")),
			{Summary: "top-level summary < & \"quote\""},
			session.NewMessageItem(session.NewAgentMessage("planner", &chat.Message{
				Role: chat.MessageRoleAssistant, Content: "assistant text > & \"quoted\" / 東京",
				ReasoningContent: "reasoning <&>", CreatedAt: stampText,
			})),
			session.NewSubSessionItem(child),
			session.NewErrorItem(&session.Error{Message: "nested-safe error <&> \"quoted\"", Code: "fixture_error", AgentName: "planner", CreatedAt: stampText}),
			session.NewTerminationItem(&session.Termination{Reason: session.TerminationReasonBudgetExceeded, Budget: "fixture", Limit: "max_tokens", Used: "11", Max: "10", Message: "budget <&> \"quoted\""}),
		}),
	)

	emptyNil := session.New(
		session.WithID("session-proof-empty-nil"), session.WithClock(func() time.Time { return stamp }),
		session.WithTitle("Nil messages become [] in MessagesSnapshot"), session.WithMessages(nil),
	)
	emptyAllocated := session.New(
		session.WithID("session-proof-empty-array"), session.WithClock(func() time.Time { return stamp }),
		session.WithTitle("Allocated empty message array"), session.WithMessages(make([]session.Item, 0)),
	)

	cases := make([]branchProofCaseFixture, 0, 3)
	for _, test := range []struct {
		name      string
		sess      *session.Session
		positions []int
		chunked   bool
	}{
		{name: "nested_summary_error_termination_escaped_unicode", sess: rich, positions: []int{0, 2, 5, 7}},
		{name: "chunked_same_raw_snapshot_and_proof", sess: rich, positions: []int{2, 5}, chunked: true},
		{name: "nil_messages_snapshot_uses_empty_array_proof", sess: emptyNil, positions: []int{0}},
		{name: "allocated_empty_array", sess: emptyAllocated, positions: []int{0}},
	} {
		proofMessages, err := json.Marshal(test.sess.MessagesSnapshot())
		if err != nil {
			return fmt.Errorf("marshal %s proof input: %w", test.name, err)
		}
		proof := runtime.SnapshotProof(test.sess)
		if want := sha256Hex(proofMessages); proof != want {
			return fmt.Errorf("runtime proof mismatch for %s: got %s want %s", test.name, proof, want)
		}
		snapshotSession := test.sess
		if test.chunked {
			snapshotSession = test.sess.Clone()
			snapshotSession.SetTitle(strings.Repeat("x", 64<<10))
		}
		snap := snapshot{
			ParentSessionID: snapshotSession.ParentID,
			Session:         snapshotSession,
			Status:          api.SessionStatus[runtime.SessionState]{SessionID: test.sess.ID, AgentName: "planner", State: runtime.SessionStateSettled, Pending: 0},
			Interactions:    []api.SessionInteraction[runtime.InteractionKind, any]{},
			PendingInputs:   []api.SessionPendingInput{}, Epoch: "branch-proof-epoch", Cursor: 7,
			TranscriptPosition: len(test.sess.MessagesSnapshot()),
		}
		snapshotBody, err := json.Marshal(snap)
		if err != nil {
			return fmt.Errorf("marshal %s snapshot: %w", test.name, err)
		}
		snapshotMessages, err := rawSnapshotMessages(snapshotBody)
		if err != nil {
			return fmt.Errorf("extract %s snapshot messages: %w", test.name, err)
		}
		// A nil stored Messages field serializes as null in Session JSON, while
		// MessagesSnapshot deliberately normalizes it to a nonnil empty slice.
		// The client must hash [] for this case; null itself is not proof input.
		snapshotMessagesNull := bytes.Equal(snapshotMessages, []byte("null"))
		proofMessagesEmpty := bytes.Equal(proofMessages, []byte("[]"))
		if !bytes.Equal(snapshotMessages, proofMessages) && !(snapshotMessagesNull && proofMessagesEmpty) {
			return fmt.Errorf("snapshot/proof message bytes differ for %s", test.name)
		}
		requests := make([]api.SessionBranchRequest, 0, len(test.positions))
		for _, position := range test.positions {
			position := position
			requests = append(requests, api.SessionBranchRequest{Position: &position, ExpectedSnapshot: proof})
		}
		fixture := branchProofCaseFixture{
			Name: test.name, SnapshotRawSHA256: sha256Hex(snapshotBody), SnapshotMessagesRaw: string(snapshotMessages),
			SnapshotMessagesNull: snapshotMessagesNull, ProofMessagesRaw: string(proofMessages),
			ProofMessagesEmpty: proofMessagesEmpty, ExpectedSnapshot: proof, Requests: requests,
			BranchProofAvailable: !snapshotMessagesNull,
		}
		if snapshotMessagesNull {
			fixture.BranchUnavailableReason = "snapshot session.messages is null; client fails closed rather than treating null bytes as the empty-array proof input"
			fixture.Requests = []api.SessionBranchRequest{}
		}
		if !test.chunked {
			fixture.SnapshotRaw = string(snapshotBody)
		}
		if test.chunked {
			frames, err := snapshotChunkFrames(snapshotBody, snap.Cursor)
			if err != nil {
				return fmt.Errorf("chunk %s snapshot: %w", test.name, err)
			}
			fixture.ChunkFrames = frames
		}
		cases = append(cases, fixture)
	}
	return writeJSON("branch_snapshot_proof_cases.json", cases)
}

func snapshotChunkFrames(data []byte, cursor uint64) ([]streamMessage, error) {
	if len(data) <= 64<<10 {
		return nil, fmt.Errorf("snapshot is only %d bytes; expected a chunked example", len(data))
	}
	frames := []streamMessage{{Version: api.SessionAPIVersion, Type: "snapshot_begin", Cursor: cursor}}
	for len(data) > 0 {
		n := min(len(data), 64<<10)
		frames = append(frames, streamMessage{Version: api.SessionAPIVersion, Type: "snapshot_chunk", Cursor: cursor, Chunk: data[:n]})
		data = data[n:]
	}
	frames = append(frames, streamMessage{Version: api.SessionAPIVersion, Type: "snapshot_end", Cursor: cursor})
	return frames, nil
}

func rawSnapshotMessages(snapshotBytes []byte) (json.RawMessage, error) {
	var envelope struct {
		Session json.RawMessage `json:"session"`
	}
	if err := json.Unmarshal(snapshotBytes, &envelope); err != nil {
		return nil, err
	}
	var sess struct {
		Messages json.RawMessage `json:"messages"`
	}
	if err := json.Unmarshal(envelope.Session, &sess); err != nil {
		return nil, err
	}
	if len(sess.Messages) == 0 {
		return nil, errors.New("snapshot session lacks messages field")
	}
	return sess.Messages, nil
}

func writeToolToggleSemantics() error {
	initial := session.New(
		session.WithID("session-tool-toggle"), session.WithClock(func() time.Time { return stamp }),
		session.WithTitle("Tool toggle fixture"), session.WithSafetyPolicy(session.SafetyPolicyBalanced),
	)
	states := []toolToggleState{toolToggleSnapshot("initial_balanced", initial)}
	initial.ToggleYolo()
	states = append(states, toolToggleSnapshot("toggle_on_remembers_balanced", initial))
	initial.ToggleYolo()
	states = append(states, toolToggleSnapshot("toggle_off_restores_balanced", initial))
	initial.ToggleYolo()
	states = append(states, toolToggleSnapshot("toggle_on_remembers_balanced_again", initial))
	initial.SetSafetyPolicy(session.SafetyPolicyStrict)
	states = append(states, toolToggleSnapshot("explicit_policy_change_clears_prior_mode", initial))
	initial.ToggleYolo()
	states = append(states, toolToggleSnapshot("toggle_on_after_policy_change_remembers_strict", initial))
	initial.ToggleYolo()
	states = append(states, toolToggleSnapshot("toggle_off_restores_new_strict_mode", initial))
	data, err := json.Marshal(states)
	if err != nil {
		return err
	}
	return writeJSON("tool_toggle_semantics.json", toolToggleSemanticsFixture{
		Endpoint: "/api/v2/sessions/:id/tools/toggle", Method: "POST", Bodyless: true,
		Response: json.RawMessage("null"), States: data,
	})
}

func toolToggleSnapshot(name string, sess *session.Session) toolToggleState {
	data, err := json.Marshal(sess)
	if err != nil {
		panic(err)
	}
	return toolToggleState{Name: name, State: data}
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func makeSession() *session.Session {
	previous := session.NewAgentMessage("root", &chat.Message{
		Role: chat.MessageRoleAssistant, Content: "I found one issue.",
		ReasoningContent: "Check the boundary condition.",
		Presentation:     []chat.AssistantPart{{Type: chat.AssistantPartReasoning, Text: "Check the boundary condition."}, {Type: chat.AssistantPartContent, Text: "I found one issue."}},
		CreatedAt:        stampText, Model: "demo/model", Cost: 0.0125,
	})
	sess := session.New(
		session.WithID(sessionID), session.WithClock(func() time.Time { return stamp }),
		session.WithTitle("Fixture conversation"), session.WithWorkingDir("/workspace/demo"),
		session.WithAgentName("root"), session.WithSafetyPolicy(session.SafetyPolicyBalanced),
		session.WithAttributes(map[string]string{"docker-agent.actor.source": "demo-team", "docker-agent.actor.agent": "root"}),
		session.WithMessages([]session.Item{
			session.NewMessageItem(session.UserMessageAt(stamp, "Review the sample changes.")),
			session.NewMessageItem(previous),
		}),
	)
	sess.Starred = true
	sess.SetTokensAndCost(120, 48, 0.0125)
	return sess
}

func workspaceSnapshotProof(id string, infos []builtins.SnapshotInfo) string {
	data, err := json.Marshal(struct {
		ID    string
		Infos []builtins.SnapshotInfo
	}{ID: id, Infos: infos})
	if err != nil {
		panic(err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func fixtureTool() (tools.ToolCall, tools.Tool) {
	call := tools.ToolCall{ID: "tool-call-001", Type: tools.ToolType("function"), Function: tools.FunctionCall{Name: "read_file", Arguments: `{"path":"README.md"}`}}
	definition := tools.Tool{Name: "read_file", Category: "filesystem", Description: "Read a file", Parameters: map[string]any{"type": "object"}, Annotations: tools.ToolAnnotations{ReadOnlyHint: true}}
	return call, definition
}
func agentContext() runtime.AgentContext {
	return runtime.AgentContext{AgentName: "root", Timestamp: stamp}
}
func intPtr(v int) *int { return &v }

func writeEventStream(snap snapshot, call tools.ToolCall, tool tools.Tool, approval *runtime.ToolCallConfirmationEvent) error {
	var b strings.Builder
	write := func(typ string, msg streamMessage, seq uint64) error {
		data, err := json.Marshal(msg)
		if err != nil {
			return err
		}
		if seq > 0 {
			fmt.Fprintf(&b, "id: %d\n", seq)
		}
		fmt.Fprintf(&b, "event: %s\ndata: %s\n\n", typ, data)
		return nil
	}
	if err := write("snapshot", streamMessage{Version: api.SessionAPIVersion, Type: "snapshot", Snapshot: &snap}, 0); err != nil {
		return err
	}
	if err := write("ready", streamMessage{Version: api.SessionAPIVersion, Type: "ready", Cursor: snap.Cursor}, 0); err != nil {
		return err
	}
	usage := 0.0125
	tree := subagent.Snapshot{Version: subagent.SnapshotVersion, Durability: subagent.DurabilityDurable, Root: subagent.SessionRootID(sessionID), Nodes: []subagent.NodeSnapshot{{Node: subagent.Node{ID: subagent.SessionRootID(sessionID), Agent: "root", SessionID: sessionID, State: subagent.NodeRunning, NeedsAttention: true, WaitingOn: "approval", Cost: 0.0125, InputTokens: 120, OutputTokens: 48, CreatedAt: stamp, UpdatedAt: stamp}}}}
	sequence := uint64(0)
	emit := func(event runtime.Event, transcript int, turn, interaction string) error {
		sequence++
		env := envelope{Version: api.SessionAPIVersion, SessionID: sessionID, TurnID: turn, InteractionID: interaction, Epoch: epoch, Sequence: sequence, TranscriptPosition: transcript, Event: event}
		return write("event", streamMessage{Version: api.SessionAPIVersion, Type: "event", Envelope: &env}, sequence)
	}
	events := []struct {
		event       runtime.Event
		pos         int
		interaction string
	}{
		{&runtime.StreamStartedEvent{AgentContext: agentContext(), Type: "stream_started", SessionID: sessionID}, -1, ""},
		{&runtime.UserMessageEvent{AgentContext: agentContext(), Type: "user_message", Message: "Please inspect README.md.", SessionID: sessionID, TurnID: turnID, SessionPosition: 2}, 2, ""},
		{&runtime.AgentChoiceReasoningEvent{AgentContext: agentContext(), Type: "agent_choice_reasoning", Content: "I should inspect the requested file.", SessionID: sessionID}, -1, ""},
		{&runtime.AgentChoiceEvent{AgentContext: agentContext(), Type: "agent_choice", Content: "I will inspect README.md.", SessionID: sessionID}, -1, ""},
		{&runtime.PartialToolCallEvent{AgentContext: agentContext(), Type: "partial_tool_call", ToolCall: call, ToolDefinition: &tool}, -1, ""},
		{&runtime.ToolCallEvent{AgentContext: agentContext(), Type: "tool_call", ToolCall: call, ToolDefinition: tool}, -1, ""},
		{approval, -1, "interaction-001"},
		{&runtime.InteractionResolvedEvent{AgentContext: agentContext(), Type: "interaction_resolved", SessionID: sessionID, InteractionID: "interaction-001", Reason: runtime.InteractionResponded}, -1, "interaction-001"},
		{&runtime.ToolCallOutputEvent{AgentContext: agentContext(), Type: "tool_call_output", ToolCallID: call.ID, ToolDefinition: tool, Output: "# Demo\n"}, -1, ""},
		{&runtime.ToolCallResponseEvent{AgentContext: agentContext(), Type: "tool_call_response", ToolCallID: call.ID, ToolDefinition: tool, Response: "# Demo\n", Result: &tools.ToolCallResult{Output: "# Demo\n"}}, -1, ""},
		{&runtime.AgentChoiceEvent{AgentContext: agentContext(), Type: "agent_choice", Content: "README.md documents the demo.\n", SessionID: sessionID}, -1, ""},
		{&runtime.MessageAddedEvent{AgentContext: agentContext(), Type: "message_added", SessionID: sessionID, SessionPosition: 3, MessageRole: chat.MessageRoleAssistant, ToolCallIDs: []string{call.ID}}, 3, ""},
		{&runtime.TokenUsageEvent{AgentContext: agentContext(), Type: "token_usage", SessionID: sessionID, Usage: &runtime.Usage{InputTokens: 120, OutputTokens: 48, ContextLength: 900, ContextLimit: 8192, Cost: usage, OwnCost: &usage, CompactionThreshold: 0.8}}, -1, ""},
		{&runtime.SubagentTreeEvent{AgentContext: agentContext(), Type: "subagent_tree", Snapshot: tree}, -1, ""},
		{&runtime.StreamStoppedEvent{AgentContext: agentContext(), Type: "stream_stopped", SessionID: sessionID}, -1, ""},
		{&runtime.TurnSettledEvent{AgentContext: agentContext(), Type: "turn_settled", SessionID: sessionID, TurnID: turnID, Outcome: runtime.TurnCompleted}, -1, ""},
	}
	for _, item := range events {
		if err := emit(item.event, item.pos, turnID, item.interaction); err != nil {
			return err
		}
	}
	return os.WriteFile(filepath.Join(outDir, "event_sequence.sse"), []byte(b.String()), 0644)
}

func writeChunkedSnapshot() error {
	large := session.New(session.WithID("session-demo-large"), session.WithClock(func() time.Time { return stamp }), session.WithTitle(strings.Repeat("fixture-large-", 10000)))
	largeSnap := snapshot{Session: large, Status: api.SessionStatus[runtime.SessionState]{SessionID: large.ID, AgentName: "root", State: runtime.SessionStateSettled, Pending: 0}, Interactions: []api.SessionInteraction[runtime.InteractionKind, any]{}, PendingInputs: []api.SessionPendingInput{}, Epoch: "fixture-epoch-large", Cursor: 4, TranscriptPosition: 0}
	data, err := json.Marshal(largeSnap)
	if err != nil {
		return err
	}
	if len(data) <= 64<<10 {
		return fmt.Errorf("large snapshot only %d bytes", len(data))
	}
	messages := []streamMessage{{Version: api.SessionAPIVersion, Type: "snapshot_begin", Cursor: largeSnap.Cursor}}
	for rest := data; len(rest) > 0; {
		n := min(len(rest), 64<<10)
		messages = append(messages, streamMessage{Version: api.SessionAPIVersion, Type: "snapshot_chunk", Cursor: largeSnap.Cursor, Chunk: rest[:n]})
		rest = rest[n:]
	}
	messages = append(messages, streamMessage{Version: api.SessionAPIVersion, Type: "snapshot_end", Cursor: largeSnap.Cursor})
	if err := writeJSON("snapshot_chunk_frames.json", messages); err != nil {
		return err
	}
	var b strings.Builder
	for _, message := range messages {
		frame, err := json.Marshal(message)
		if err != nil {
			return err
		}
		fmt.Fprintf(&b, "event: %s\ndata: %s\n\n", message.Type, frame)
	}
	if err := os.WriteFile(filepath.Join(outDir, "snapshot_chunked.sse"), []byte(b.String()), 0644); err != nil {
		return err
	}
	return nil
}

func writeJSON(name string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return os.WriteFile(filepath.Join(outDir, name), data, 0644)
}

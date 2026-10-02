package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/docker/docker-agent/pkg/api"
	"github.com/docker/docker-agent/pkg/config/types"
	"github.com/docker/docker-agent/pkg/session"
	"github.com/docker/docker-agent/pkg/tools"
)

type SessionToolsInfo = api.SessionToolsInfo
type SessionToolsetStatus = api.SessionToolsetStatus

type SessionToolInspector interface {
	InspectTools(context.Context) (SessionToolsInfo, error)
}
type SessionToolsetController interface {
	RestartToolset(context.Context, string) error
}
type SessionPermissionsInspector interface {
	EffectivePermissions(context.Context) (SessionPermissionsInfo, error)
}
type SessionPermissionsInfo struct {
	Policy        session.SafetyPolicy       `json:"policy"`
	ToolsApproved bool                       `json:"tools_approved"`
	Source        *PermissionsInfo           `json:"source,omitempty"`
	Session       *session.PermissionsConfig `json:"session,omitempty"`
}
type SessionMCPPrompts interface {
	MCPPrompts(context.Context) (map[string]tools.PromptInfo, error)
	ExecuteMCPPrompt(context.Context, string, map[string]string) (string, error)
}
type BranchOptions = api.SessionBranchRequest
type SessionBrancher interface {
	BranchSession(context.Context, string, BranchOptions) (SessionHandle, *session.Session, error)
}

// SnapshotProof proves a positional cut refers to exactly the displayed item
// sequence, including summaries and embedded sub-sessions. Safety and other
// metadata are deliberately excluded: branches inherit their current values.
func SnapshotProof(sess *session.Session) string {
	if sess == nil {
		return ""
	}
	data, err := json.Marshal(sess.MessagesSnapshot())
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func (h *sessionHandle) InspectTools(ctx context.Context) (SessionToolsInfo, error) {
	available, err := h.runtime.AgentTools(ctx, h.agentName)
	if err != nil {
		return SessionToolsInfo{}, err
	}
	// Descriptors must never grant execution authority to callers.
	available = slices.Clone(available)
	for i := range available {
		available[i].Handler = nil
	}
	statuses := h.runtime.AgentToolsetStatuses(h.agentName)
	out := SessionToolsInfo{Tools: available, Statuses: make([]SessionToolsetStatus, 0, len(statuses))}
	for _, status := range statuses {
		safeError := ""
		if status.LastError != nil {
			safeError = "toolset unavailable"
		}
		out.Statuses = append(out.Statuses, SessionToolsetStatus{Name: status.Name, Kind: status.Kind, Description: status.Description, State: status.State, LastError: safeError, RestartCount: status.RestartCount, Restartable: status.Restartable})
	}
	return out, nil
}
func (h *sessionHandle) RestartToolset(ctx context.Context, name string) error {
	a, err := h.runtime.team.Agent(h.agentName)
	if err != nil {
		return err
	}
	for _, ts := range a.ToolSets() {
		if nameFor(ts, tools.DescribeToolSet(ts)) == name {
			if restartable, ok := tools.As[tools.Restartable](ts); ok {
				return restartable.Restart(ctx)
			}
		}
	}
	return sessionUnsupported(h.ID(), "restart_toolset")
}
func (h *sessionHandle) EffectivePermissions(ctx context.Context) (SessionPermissionsInfo, error) {
	if err := ctx.Err(); err != nil {
		return SessionPermissionsInfo{}, err
	}
	approved, policy, rules := h.driver.session().SafetySettings()
	policy = policy.Normalize()
	if policy == "" && approved {
		policy = session.SafetyPolicyAutonomous
	}
	return SessionPermissionsInfo{Policy: policy, ToolsApproved: approved, Source: h.runtime.PermissionsInfo(), Session: rules}, nil
}

type boundPrompt struct {
	info    tools.PromptInfo
	toolset mcpPromptToolset
}

func (h *sessionHandle) boundPrompts(ctx context.Context) (map[string]boundPrompt, error) {
	a, err := h.runtime.team.Agent(h.agentName)
	if err != nil {
		return nil, err
	}
	result := make(map[string]boundPrompt)
	// Qualify every discovery key by its immutable bound-agent toolset index.
	// Names may contain slashes; execution passes the original name unchanged.
	for i, ts := range a.ToolSets() {
		if provider, ok := tools.As[mcpPromptToolset](ts); ok {
			prompts, err := provider.ListPrompts(ctx)
			if err != nil {
				return nil, err
			}
			for _, prompt := range prompts {
				result[strconv.Itoa(i)+"/"+prompt.Name] = boundPrompt{prompt, provider}
			}
		}
	}
	return result, nil
}
func (h *sessionHandle) MCPPrompts(ctx context.Context) (map[string]tools.PromptInfo, error) {
	prompts, err := h.boundPrompts(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[string]tools.PromptInfo, len(prompts))
	for key, prompt := range prompts {
		out[key] = prompt.info
	}
	return out, nil
}
func (h *sessionHandle) ExecuteMCPPrompt(ctx context.Context, key string, args map[string]string) (string, error) {
	prompts, err := h.boundPrompts(ctx)
	if err != nil {
		return "", err
	}
	prompt, ok := prompts[key]
	if !ok {
		return "", &SessionError{Kind: SessionErrorNotFound, SessionID: h.ID(), Operation: "mcp_prompt"}
	}
	result, err := prompt.toolset.GetPrompt(ctx, prompt.info.Name, args)
	if err != nil {
		return "", err
	}
	if result == nil || len(result.Messages) == 0 {
		return "No content returned from MCP prompt", nil
	}
	var content strings.Builder
	for i, msg := range result.Messages {
		if i > 0 {
			content.WriteString("\n\n")
		}
		if text, ok := msg.Content.(*mcp.TextContent); ok {
			content.WriteString(text.Text)
		} else {
			fmt.Fprintf(&content, "[Non-text content: %T]", msg.Content)
		}
	}
	return content.String(), nil
}

func (v *localSessionRuntimeView) BranchSession(ctx context.Context, id string, options BranchOptions) (SessionHandle, *session.Session, error) {
	driver, ok := v.runtime.sessionDrivers.Lookup(id)
	if !ok {
		return nil, nil, &SessionError{Kind: SessionErrorNotFound, SessionID: id, Operation: "branch"}
	}
	// Existing switch admission supplies a locked, settled root snapshot. No
	// turn can mutate the item sequence while this snapshot is being taken.
	original, agentName, model, err := driver.cloneForBranch(ctx)
	if err != nil {
		return nil, nil, err
	}
	position := len(original.MessagesSnapshot())
	if options.Position != nil {
		if options.ExpectedSnapshot == "" || options.ExpectedSnapshot != SnapshotProof(original) {
			return nil, nil, &SessionError{Kind: SessionErrorStale, SessionID: id, Operation: "branch"}
		}
		position = *options.Position
	}
	if position < 0 || position > len(original.MessagesSnapshot()) {
		return nil, nil, &SessionError{Kind: SessionErrorInvalid, SessionID: id, Operation: "branch"}
	}
	branched, err := session.BranchSession(original, position)
	if err != nil {
		return nil, nil, err
	}
	branched.AgentName = agentName
	if model != "" {
		if branched.AgentModelOverrides == nil {
			branched.AgentModelOverrides = make(map[string]string)
		}
		branched.AgentModelOverrides[agentName] = model
	}
	branched.SetAttribute(SessionAgentAttribute, branched.AgentName)
	handle, err := v.CreateSession(ctx, branched, SessionBinding{AgentName: branched.AgentName, Model: model})
	return handle, branched, err
}
func (r *LocalRuntime) BranchSession(ctx context.Context, id string, options BranchOptions) (SessionHandle, *session.Session, error) {
	return (&localSessionRuntimeView{runtime: r}).BranchSession(ctx, id, options)
}

// SessionViewInfoReader confirms persisted identity and view policy without
// reserving, publishing, restoring, or starting canonical execution.
// Runtime decorators must forward it with the same owner lifetime as preparation.
type SessionViewInfoReader interface {
	ConfirmedSessionViewInfo(ctx context.Context, sessionID string) (PreparedSessionViewInfo, error)
}

var (
	_ SessionViewInfoReader = (*localSessionRuntimeView)(nil)
	_ SessionViewPreparer   = (*localSessionRuntimeView)(nil)
)

// cloneForBranch captures model and transcript under the same owner lock.
func (d *sessionDriver) cloneForBranch(ctx context.Context) (*session.Session, string, string, error) {
	if err := ctx.Err(); err != nil {
		return nil, "", "", err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.admitLocked(SessionOperationSwitchAgent); err != nil {
		return nil, "", "", err
	}
	if d.sess == nil || d.sess.ParentID != "" {
		return nil, "", "", sessionUnsupported(d.sessionIDLocked(), "branch")
	}
	snapshot := d.sess.Clone()
	boundAgent := snapshot.AttributesSnapshot()[SessionAgentAttribute]
	if boundAgent == "" {
		boundAgent = d.AgentNameLocked()
	}
	return snapshot, boundAgent, d.modelRef, nil
}

// SessionAgentInfoProvider supplies bound presentation metadata without exposing
// global runtime selection. UI callers fetch this asynchronously and cache it.
type SessionAgentInfoProvider interface {
	SessionAgentInfo(context.Context) (SessionAgentInfo, error)
}
type SessionAgentInfo struct {
	Agent    *AgentInfoEvent `json:"agent,omitempty"`
	Team     *TeamInfoEvent  `json:"team,omitempty"`
	Commands types.Commands  `json:"commands,omitempty"`
}

func (h *sessionHandle) SessionAgentInfo(ctx context.Context) (SessionAgentInfo, error) {
	if err := ctx.Err(); err != nil {
		return SessionAgentInfo{}, err
	}
	a, err := h.runtime.team.Agent(h.agentName)
	if err != nil {
		return SessionAgentInfo{}, err
	}
	out := SessionAgentInfo{Commands: maps.Clone(a.Commands())}
	h.EmitPinnedAgentInfo(ctx, EventSinkFunc(func(event Event) {
		switch event := event.(type) {
		case *AgentInfoEvent:
			out.Agent = event
		case *TeamInfoEvent:
			out.Team = event
		}
	}))
	return out, ctx.Err()
}

// SessionIDCreator creates a fresh root with an explicit caller-selected ID.
// Ordinary CreateSession leaves identity allocation to the runtime. Transports
// must negotiate this optional capability before issuing a create mutation.
type SessionIDCreator interface {
	CreateSessionWithID(context.Context, *session.Session, SessionBinding, string) (SessionHandle, error)
}

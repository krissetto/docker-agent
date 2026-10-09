package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/docker/docker-agent/pkg/api"
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/version"
)

type SendInput struct {
	Message   string `json:"message" jsonschema:"the message to send"`
	FollowUp  bool   `json:"followup,omitempty" jsonschema:"queue as end-of-turn follow-up instead of mid-turn steer"`
	RequestID string `json:"request_id,omitempty" jsonschema:"idempotency key for retrying admission"`
}

type SendOutput = runtime.Submission

type TranscriptInput struct {
	Limit int `json:"limit,omitempty" jsonschema:"max number of recent messages to return (0 = all)"`
}

type TranscriptOutput struct {
	Title    string        `json:"title"`
	Messages []TranscriptM `json:"messages"`
}

type TranscriptM struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type AttachOptions struct {
	ClientOptions []runtime.ClientOption
	ConfirmChild  bool
}

type AttachedReadOutput struct {
	SessionID    string                                                            `json:"session_id"`
	Title        string                                                            `json:"title"`
	Epoch        string                                                            `json:"epoch"`
	Cursor       uint64                                                            `json:"cursor"`
	Status       api.SessionStatus[runtime.SessionState]                           `json:"status"`
	Capabilities AttachCapabilities                                                `json:"capabilities"`
	Interactions []api.SessionInteraction[runtime.InteractionKind, map[string]any] `json:"interactions"`
}

type AttachCapabilities struct {
	StopSubtree      bool `json:"stop_subtree"`
	DelegationPolicy bool `json:"delegation_policy"`
}

type AttachedTurnInput struct {
	TurnID string `json:"turn_id" jsonschema:"exact accepted turn ID; required, never session-wide"`
}

type AttachedControlOutput struct {
	SessionID string `json:"session_id"`
}

type AttachedPolicyInput struct {
	Enabled *bool `json:"enabled,omitempty" jsonschema:"omit to read; set to update delegation policy"`
}

type AttachedPolicyOutput struct {
	SessionID string `json:"session_id"`
	Enabled   bool   `json:"enabled"`
}

// AttachServer borrows a canonical authority. Disconnecting never cancels its work.
func AttachServer(ctx context.Context, addr, sessionID string) (*mcp.Server, error) {
	return AttachServerWithOptions(ctx, addr, sessionID, AttachOptions{})
}

func AttachServerWithOptions(ctx context.Context, addr, sessionID string, options AttachOptions) (*mcp.Server, error) {
	if addr == "" || sessionID == "" {
		return nil, errors.New("attach: addr and sessionID are required")
	}
	u, err := url.Parse(addr)
	if err != nil || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, errors.New("attach: authority URL must not contain credentials, query or fragment")
	}
	// Never forward session input or credentials to a redirected authority.
	clientOptions := append([]runtime.ClientOption{runtime.WithHTTPClient(&http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }})}, options.ClientOptions...)
	client, err := runtime.NewClient(addr, clientOptions...)
	if err != nil {
		return nil, errors.New("attach: invalid authority address")
	}
	transport, err := runtime.NewSessionTransport(client)
	if err != nil {
		return nil, err
	}
	snapshot, err := client.ReadSessionSnapshot(ctx, sessionID)
	if err != nil {
		return nil, fmt.Errorf("attach snapshot: %w", err)
	}
	handle, err := transport.SessionByID(sessionID)
	if err != nil {
		return nil, err
	}
	if snapshot.Session.ParentID != "" {
		if !options.ConfirmChild {
			return nil, errors.New("child attachment requires explicit confirmation")
		}
		prepared, err := transport.PrepareSessionView(ctx, sessionID)
		if err != nil {
			return nil, err
		}
		defer prepared.Abort()
		committed, err := prepared.Commit(ctx)
		if err != nil {
			return nil, err
		}
		handle = committed.SessionHandle
	}
	if _, err := handle.Status(ctx); err != nil {
		return nil, err
	}
	capabilities := handle.Metadata().Capabilities
	server := mcp.NewServer(&mcp.Implementation{Name: "docker-agent (attached)", Version: version.Version}, nil)

	mcp.AddTool(server, &mcp.Tool{Name: "send", Description: "Admit input asynchronously. Returns canonical session/turn IDs and disposition. Detach does not cancel accepted input; use await_turn or cancel_turn explicitly."}, func(ctx context.Context, _ *mcp.CallToolRequest, in SendInput) (*mcp.CallToolResult, SendOutput, error) {
		input := runtime.TurnInput{Content: in.Message, RequestID: in.RequestID}
		var accepted runtime.Submission
		var err error
		if in.FollowUp {
			accepted, err = handle.Submit(ctx, input)
		} else {
			accepted, err = handle.Steer(ctx, input)
		}
		return nil, accepted, err
	})

	mcp.AddTool(server, &mcp.Tool{Name: "read", Description: "Read canonical status and current interactions, including exact request tokens and elicitation form schemas. Does not respond or approve."}, func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, AttachedReadOutput, error) {
		snapshot, err := client.ReadSessionSnapshot(ctx, sessionID)
		if err != nil {
			return nil, AttachedReadOutput{}, err
		}
		out := AttachedReadOutput{SessionID: sessionID, Title: snapshot.Session.TitleSnapshot(), Epoch: snapshot.Epoch, Cursor: snapshot.Cursor, Status: api.SessionStatus[runtime.SessionState](snapshot.Status), Capabilities: AttachCapabilities{StopSubtree: capabilities.StopSubtree, DelegationPolicy: capabilities.DelegationPolicy}, Interactions: []api.SessionInteraction[runtime.InteractionKind, map[string]any]{}}
		for _, interaction := range snapshot.Interactions {
			data, err := json.Marshal(interaction.Event)
			if err != nil {
				return nil, AttachedReadOutput{}, err
			}
			var event map[string]any
			if err := json.Unmarshal(data, &event); err != nil {
				return nil, AttachedReadOutput{}, err
			}
			out.Interactions = append(out.Interactions, api.SessionInteraction[runtime.InteractionKind, map[string]any]{SessionID: interaction.SessionID, InteractionID: interaction.InteractionID, Kind: interaction.Kind, ElicitationID: interaction.ElicitationID, Event: event})
		}
		return nil, out, nil
	})

	mcp.AddTool(server, &mcp.Tool{Name: "respond", Description: "Resolve one current interaction from read using its exact interaction_id and kind. For elicitation include elicitation_id, action (accept/decline/cancel) and form content; confirmation/max_iterations use confirmation (approve/reject). Stale tokens are rejected by the authority."}, func(ctx context.Context, _ *mcp.CallToolRequest, in api.SessionResponseRequest[runtime.InteractionKind]) (*mcp.CallToolResult, AttachedControlOutput, error) {
		if in.InteractionID == "" || in.Kind == "" {
			return nil, AttachedControlOutput{}, errors.New("interaction_id and kind are required")
		}
		err := handle.Respond(ctx, runtime.InteractionResponse{InteractionID: in.InteractionID, Kind: in.Kind, Resume: runtime.ResumeRequest{Type: runtime.ResumeType(in.Confirmation), Reason: in.Reason, ToolName: in.ToolName}, ElicitationID: in.ElicitationID, Elicitation: runtime.ElicitationResult{Action: tools.ElicitationAction(in.Action), Content: in.Content}, ClientID: in.ClientID})
		return nil, AttachedControlOutput{SessionID: sessionID}, err
	})

	mcp.AddTool(server, &mcp.Tool{Name: "await_turn", Description: "Wait for settlement of an exact accepted turn. Cancelling this wait only detaches; it does not cancel the turn."}, func(ctx context.Context, _ *mcp.CallToolRequest, in AttachedTurnInput) (*mcp.CallToolResult, AttachedControlOutput, error) {
		if in.TurnID == "" {
			return nil, AttachedControlOutput{}, errors.New("turn_id is required")
		}
		return nil, AttachedControlOutput{SessionID: sessionID}, handle.AwaitTurn(ctx, in.TurnID)
	})
	mcp.AddTool(server, &mcp.Tool{Name: "cancel_turn", Description: "Cancel only the exact supplied turn ID, never replacement or unrelated work. Await settlement separately."}, func(ctx context.Context, _ *mcp.CallToolRequest, in AttachedTurnInput) (*mcp.CallToolResult, runtime.CancelResult, error) {
		if in.TurnID == "" {
			return nil, runtime.CancelResult{}, errors.New("turn_id is required")
		}
		out, err := handle.Cancel(ctx, in.TurnID)
		return nil, out, err
	})

	if capabilities.StopSubtree {
		mcp.AddTool(server, &mcp.Tool{Name: "stop_subtree", Description: "Fence and drain this session's subtree, preserving history. This is not turn cancellation or detach."}, func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, AttachedControlOutput, error) {
			return nil, AttachedControlOutput{SessionID: sessionID}, handle.(runtime.SessionTreeController).StopSubtree(ctx)
		})
	}
	if capabilities.DelegationPolicy {
		mcp.AddTool(server, &mcp.Tool{Name: "delegation_policy", Description: "Read or update the authority's delegation policy. Only advertised when supported; child policy remains subject to canonical root rules."}, func(ctx context.Context, _ *mcp.CallToolRequest, in AttachedPolicyInput) (*mcp.CallToolResult, AttachedPolicyOutput, error) {
			controller := handle.(runtime.SessionDelegationController)
			if in.Enabled != nil {
				if err := controller.SetDelegationPolicy(ctx, *in.Enabled); err != nil {
					return nil, AttachedPolicyOutput{}, err
				}
			}
			enabled, err := controller.DelegationPolicy(ctx)
			return nil, AttachedPolicyOutput{SessionID: sessionID, Enabled: enabled}, err
		})
	}
	mcp.AddTool(server, &mcp.Tool{Name: "transcript", Description: "Read recent text messages from the canonical session transcript."}, func(ctx context.Context, _ *mcp.CallToolRequest, in TranscriptInput) (*mcp.CallToolResult, TranscriptOutput, error) {
		sess, err := handle.Snapshot(ctx)
		if err != nil {
			return nil, TranscriptOutput{}, err
		}
		out := TranscriptOutput{Title: sess.TitleSnapshot(), Messages: []TranscriptM{}}
		messages := sess.Messages
		if in.Limit > 0 && len(messages) > in.Limit {
			messages = messages[len(messages)-in.Limit:]
		}
		for _, message := range messages {
			if message.Message == nil {
				continue
			}
			out.Messages = append(out.Messages, TranscriptM{Role: string(message.Message.Message.Role), Content: message.Message.Message.Content})
		}
		return nil, out, nil
	})
	return server, nil
}

func StartAttachStdio(ctx context.Context, addr, sessionID string) error {
	return StartAttachStdioWithOptions(ctx, addr, sessionID, AttachOptions{})
}

func StartAttachStdioWithOptions(ctx context.Context, addr, sessionID string, options AttachOptions) error {
	server, err := AttachServerWithOptions(ctx, addr, sessionID, options)
	if err != nil {
		return err
	}
	return server.Run(ctx, &mcp.StdioTransport{})
}

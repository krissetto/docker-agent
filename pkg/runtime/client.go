package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"time"

	"github.com/docker/docker-agent/pkg/api"
	"github.com/docker/docker-agent/pkg/config/latest"
	"github.com/docker/docker-agent/pkg/session"
)

// Client is an HTTP client for the docker agent server API
type Client struct {
	baseURL    *url.URL
	httpClient *http.Client
	authToken  string
	registry   map[string]func() Event
}

// ClientOption is a function for configuring the Client
type ClientOption func(*Client)

// WithHTTPClient sets a custom HTTP client
func WithHTTPClient(client *http.Client) ClientOption {
	return func(c *Client) {
		c.httpClient = client
	}
}

// WithAuthToken sets the bearer token for authentication
func WithAuthToken(token string) ClientOption {
	return func(c *Client) {
		c.authToken = token
	}
}

// WithTimeout sets the HTTP client timeout (deprecated: prefer per-request timeouts)
func WithTimeout(timeout time.Duration) ClientOption {
	return func(c *Client) {
		if c.httpClient == nil {
			c.httpClient = &http.Client{}
		}
		c.httpClient.Timeout = timeout
	}
}

// timeoutFor returns the appropriate timeout for a request category
func (c *Client) timeoutFor(category string) time.Duration {
	// Short timeout for metadata/CRUD operations
	if category == "metadata" || category == "crud" {
		return 30 * time.Second
	}
	// Long timeout for streaming/SSE operations
	return 5 * time.Minute
}

// NewClient creates a new HTTP client for the docker agent server
func NewClient(baseURL string, opts ...ClientOption) (*Client, error) {
	parsedURL, err := url.Parse(baseURL)
	if err != nil {
		return nil, fmt.Errorf("invalid base URL: %w", err)
	}

	client := &Client{
		baseURL: parsedURL,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
		registry: map[string]func() Event{
			"pending_user_message_edited":   func() Event { return &PendingUserMessageEditedEvent{} },
			"pending_user_message_accepted": func() Event { return &PendingUserMessageAcceptedEvent{} },
			"pending_user_message_promoted": func() Event { return &PendingUserMessagePromotedEvent{} },
			"turn_settled":                  func() Event { return &TurnSettledEvent{} },
			"subagent_created":              func() Event { return &SubagentCreatedEvent{} },
			"interaction_resolved":          func() Event { return &InteractionResolvedEvent{} },
			"pending_user_message_canceled": func() Event { return &PendingUserMessageCanceledEvent{} },
			"user_message":                  func() Event { return &UserMessageEvent{} },
			"tool_call":                     func() Event { return &ToolCallEvent{} },
			"tool_call_output":              func() Event { return &ToolCallOutputEvent{} },
			"tool_call_response":            func() Event { return &ToolCallResponseEvent{} },
			"tool_call_confirmation":        func() Event { return &ToolCallConfirmationEvent{} },
			"token_usage":                   func() Event { return &TokenUsageEvent{} },
			"stream_stopped":                func() Event { return &StreamStoppedEvent{} },
			"session_dormancy_changed":      func() Event { return &DormancyChangedEvent{} },
			"runtime_paused":                func() Event { return &PausedEvent{} },
			"runtime_pause_changed":         func() Event { return &PauseChangedEvent{} },
			"skill_operation":               func() Event { return &SkillOperationEvent{} },
			"stream_started":                func() Event { return &StreamStartedEvent{} },
			"subagent_tree":                 func() Event { return &SubagentTreeEvent{} },
			"shell":                         func() Event { return &ShellOutputEvent{} },
			"session_title":                 func() Event { return &SessionTitleEvent{} },
			"plan_changed":                  func() Event { return &PlanChangedEvent{} },
			"session_summary":               func() Event { return &SessionSummaryEvent{} },
			"session_compaction":            func() Event { return &SessionCompactionEvent{} },
			"partial_tool_call":             func() Event { return &PartialToolCallEvent{} },
			"max_iterations_reached":        func() Event { return &MaxIterationsReachedEvent{} },
			"budget_usage":                  func() Event { return &BudgetUsageEvent{} },
			"budget_exceeded":               func() Event { return &BudgetExceededEvent{} },
			"error":                         func() Event { return &ErrorEvent{} },
			"elicitation_request":           func() Event { return &ElicitationRequestEvent{} },
			"authorization_event":           func() Event { return &AuthorizationEvent{} },
			"agent_choice":                  func() Event { return &AgentChoiceEvent{} },
			"agent_choice_reasoning":        func() Event { return &AgentChoiceReasoningEvent{} },
			"mcp_init_started":              func() Event { return &MCPInitStartedEvent{} },
			"mcp_init_finished":             func() Event { return &MCPInitFinishedEvent{} },
			"agent_info":                    func() Event { return &AgentInfoEvent{} },
			"team_info":                     func() Event { return &TeamInfoEvent{} },
			"toolset_info":                  func() Event { return &ToolsetInfoEvent{} },
			"agent_switching":               func() Event { return &AgentSwitchingEvent{} },
			"warning":                       func() Event { return &WarningEvent{} },
			"hook_blocked":                  func() Event { return &HookBlockedEvent{} },
			"hook_started":                  func() Event { return &HookStartedEvent{} },
			"hook_finished":                 func() Event { return &HookFinishedEvent{} },
			"rag_indexing_started":          func() Event { return &RAGIndexingStartedEvent{} },
			"rag_indexing_progress":         func() Event { return &RAGIndexingProgressEvent{} },
			"rag_indexing_completed":        func() Event { return &RAGIndexingCompletedEvent{} },
			"message_added":                 func() Event { return &MessageAddedEvent{} },
			"model_fallback":                func() Event { return &ModelFallbackEvent{} },
			"sub_session_completed":         func() Event { return &SubSessionCompletedEvent{} },
		},
	}

	for _, opt := range opts {
		opt(client)
	}

	return client, nil
}

// ErrorResponse represents an error response from the API
type ErrorResponse struct {
	Error string `json:"error"`
}

// doRequest performs an HTTP request and handles common response patterns
func (c *Client) doRequest(ctx context.Context, method, endpoint string, body, result any) error {
	return c.doRequestWithTimeout(ctx, method, endpoint, body, result, "crud")
}

// doRequestWithTimeout performs an HTTP request with explicit timeout category
func (c *Client) doRequestWithTimeout(ctx context.Context, method, endpoint string, body, result any, timeoutCategory string) error {
	var reqBody io.Reader
	if body != nil {
		jsonBody, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("marshaling request body: %w", err)
		}
		reqBody = bytes.NewReader(jsonBody)
	}

	u := *c.baseURL
	u.Path = path.Join(u.Path, endpoint)

	// Apply per-request timeout based on category
	timeout := c.timeoutFor(timeoutCategory)
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, method, u.String(), reqBody)
	if err != nil {
		return fmt.Errorf("creating request: %w", err)
	}

	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	if c.authToken != "" {
		req.Header.Set("Authorization", "Bearer "+c.authToken)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("performing request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("reading response body: %w", err)
	}

	if resp.StatusCode >= 400 {
		var errResp ErrorResponse
		if err := json.Unmarshal(respBody, &errResp); err == nil && errResp.Error != "" {
			return fmt.Errorf("API error (%d): %s", resp.StatusCode, errResp.Error)
		}
		return fmt.Errorf("HTTP error %d: %s", resp.StatusCode, string(respBody))
	}

	if result != nil {
		if err := json.Unmarshal(respBody, result); err != nil {
			return fmt.Errorf("unmarshaling response: %w", err)
		}
	}

	return nil
}

// GetAgents retrieves all available agents
func (c *Client) GetAgents(ctx context.Context) ([]api.Agent, error) {
	var agents []api.Agent
	err := c.doRequest(ctx, http.MethodGet, "/api/agents", nil, &agents)
	return agents, err
}

// GetAgent retrieves an agent by ID
func (c *Client) GetAgent(ctx context.Context, id string) (*latest.Config, error) {
	var config latest.Config
	err := c.doRequest(ctx, http.MethodGet, "/api/agents/"+id, nil, &config)
	return &config, err
}

// CreateAgent creates a new agent using a prompt
func (c *Client) CreateAgent(ctx context.Context, prompt string) (*api.CreateAgentResponse, error) {
	req := api.CreateAgentRequest{Prompt: prompt}
	var resp api.CreateAgentResponse
	err := c.doRequest(ctx, http.MethodPost, "/api/agents", req, &resp)
	return &resp, err
}

// CreateAgentConfig creates a new agent manually with YAML configuration
func (c *Client) CreateAgentConfig(ctx context.Context, filename, model, description, instruction string) (*api.CreateAgentConfigResponse, error) {
	req := api.CreateAgentConfigRequest{
		Filename:    filename,
		Model:       model,
		Description: description,
		Instruction: instruction,
	}
	var resp api.CreateAgentConfigResponse
	err := c.doRequest(ctx, http.MethodPost, "/api/agents/config", req, &resp)
	return &resp, err
}

// EditAgentConfig edits an agent configuration
func (c *Client) EditAgentConfig(ctx context.Context, filename string, config latest.Config) (*api.EditAgentConfigResponse, error) {
	req := api.EditAgentConfigRequest{
		AgentConfig: config,
		Filename:    filename,
	}
	var resp api.EditAgentConfigResponse
	err := c.doRequest(ctx, "PUT", "/api/agents/config", req, &resp)
	return &resp, err
}

// ImportAgent imports an agent from a file path
func (c *Client) ImportAgent(ctx context.Context, filePath string) (*api.ImportAgentResponse, error) {
	req := api.ImportAgentRequest{FilePath: filePath}
	var resp api.ImportAgentResponse
	err := c.doRequest(ctx, http.MethodPost, "/api/agents/import", req, &resp)
	return &resp, err
}

// ExportAgents exports multiple agents as a zip file
func (c *Client) ExportAgents(ctx context.Context) (*api.ExportAgentsResponse, error) {
	var resp api.ExportAgentsResponse
	err := c.doRequest(ctx, http.MethodPost, "/api/agents/export", nil, &resp)
	return &resp, err
}

// PullAgent pulls an agent from a remote registry
func (c *Client) PullAgent(ctx context.Context, name string) (*api.PullAgentResponse, error) {
	req := api.PullAgentRequest{Name: name}
	var resp api.PullAgentResponse
	err := c.doRequest(ctx, http.MethodPost, "/api/agents/pull", req, &resp)
	return &resp, err
}

// PushAgent pushes an agent to a remote registry
func (c *Client) PushAgent(ctx context.Context, filepath, tag string) (*api.PushAgentResponse, error) {
	req := api.PushAgentRequest{Filepath: filepath, Tag: tag}
	var resp api.PushAgentResponse
	err := c.doRequest(ctx, http.MethodPost, "/api/agents/push", req, &resp)
	return &resp, err
}

// DeleteAgent deletes an agent by file path
func (c *Client) DeleteAgent(ctx context.Context, filePath string) (*api.DeleteAgentResponse, error) {
	req := api.DeleteAgentRequest{FilePath: filePath}
	var resp api.DeleteAgentResponse
	err := c.doRequest(ctx, "DELETE", "/api/agents", req, &resp)
	return &resp, err
}

type sessionResourceResponse struct {
	SessionID     string                     `json:"session_id"`
	Title         string                     `json:"title"`
	CreatedAt     string                     `json:"created_at"`
	Messages      []session.Message          `json:"messages,omitempty"`
	ToolsApproved bool                       `json:"tools_approved"`
	SafetyPolicy  session.SafetyPolicy       `json:"safety_policy,omitempty"`
	InputTokens   int64                      `json:"input_tokens"`
	OutputTokens  int64                      `json:"output_tokens"`
	WorkingDir    string                     `json:"working_dir,omitempty"`
	Permissions   *session.PermissionsConfig `json:"permissions,omitempty"`
}

type sessionCatalogResponse struct {
	Sessions []sessionResourceResponse `json:"sessions"`
}

// GetSessions retrieves all sessions
func (c *Client) GetSessions(ctx context.Context) ([]api.SessionsResponse, error) {
	var catalog sessionCatalogResponse
	if err := c.doRequest(ctx, http.MethodGet, "/api/sessions", nil, &catalog); err != nil {
		return nil, err
	}
	out := make([]api.SessionsResponse, len(catalog.Sessions))
	for i, resource := range catalog.Sessions {
		out[i] = api.SessionsResponse{ID: resource.SessionID, Title: resource.Title, CreatedAt: resource.CreatedAt, NumMessages: len(resource.Messages)}
	}
	return out, nil
}

// GetSession retrieves a session by ID
func (c *Client) GetSession(ctx context.Context, id string) (*api.SessionResponse, error) {
	var resource sessionResourceResponse
	if err := c.doRequest(ctx, http.MethodGet, "/api/sessions/"+id, nil, &resource); err != nil {
		return nil, err
	}
	createdAt, _ := time.Parse(time.RFC3339Nano, resource.CreatedAt)
	return &api.SessionResponse{ID: resource.SessionID, Title: resource.Title, CreatedAt: createdAt, Messages: resource.Messages, ToolsApproved: resource.ToolsApproved, SafetyPolicy: resource.SafetyPolicy, InputTokens: resource.InputTokens, OutputTokens: resource.OutputTokens, WorkingDir: resource.WorkingDir, Permissions: resource.Permissions}, nil
}

// DeleteSession deletes a session by ID
func (c *Client) DeleteSession(ctx context.Context, id string) error {
	return c.doRequest(ctx, "DELETE", "/api/sessions/"+id, nil, nil)
}

// GetDesktopToken retrieves a desktop authentication token
func (c *Client) GetDesktopToken(ctx context.Context) (*api.DesktopTokenResponse, error) {
	var resp api.DesktopTokenResponse
	err := c.doRequest(ctx, http.MethodGet, "/api/desktop/token", nil, &resp)
	return &resp, err
}

// GetAllSessions retrieves all sessions from the remote store.
func (c *Client) GetAllSessions(ctx context.Context) ([]session.Session, error) {
	var catalog sessionCatalogResponse
	if err := c.doRequest(ctx, http.MethodGet, "/api/sessions", nil, &catalog); err != nil {
		return nil, err
	}
	out := make([]session.Session, len(catalog.Sessions))
	for i, resource := range catalog.Sessions {
		createdAt, _ := time.Parse(time.RFC3339Nano, resource.CreatedAt)
		out[i] = session.Session{ID: resource.SessionID, Title: resource.Title, CreatedAt: createdAt, ToolsApproved: resource.ToolsApproved, SafetyPolicy: resource.SafetyPolicy, InputTokens: resource.InputTokens, OutputTokens: resource.OutputTokens, WorkingDir: resource.WorkingDir, Permissions: resource.Permissions}
		for j := range resource.Messages {
			message := resource.Messages[j]
			out[i].AddMessage(&message)
		}
	}
	return out, nil
}

// UpdateSessionTitle updates the title of a session
func (c *Client) UpdateSessionTitle(ctx context.Context, sessionID, title string) error {
	req := api.UpdateSessionTitleRequest{Title: title}
	return c.doRequest(ctx, http.MethodPatch, "/api/sessions/"+sessionID+"/title", req, nil)
}

// GetAgentToolCount returns the number of tools available for an agent.
func (c *Client) GetAgentToolCount(ctx context.Context, agentFilename, agentName string) (int, error) {
	var resp struct {
		AvailableTools int `json:"available_tools"`
	}
	endpoint := fmt.Sprintf("/api/agents/%s/%s/tools/count", url.PathEscape(agentFilename), url.PathEscape(agentName))
	err := c.doRequest(ctx, http.MethodGet, endpoint, nil, &resp)
	if err != nil {
		return 0, err
	}

	return resp.AvailableTools, nil
}

// GetAvailableModels returns available models for the agent.
func (c *Client) GetAvailableModels(ctx context.Context) ([]string, error) {
	var models []string
	err := c.doRequest(ctx, http.MethodGet, "/api/models", nil, &models)
	return models, err
}

// ExecuteSessionMCPPrompt executes an MCP prompt in a session.
func (c *Client) ExecuteSessionMCPPrompt(ctx context.Context, sessionID, promptName string, args map[string]string) (string, error) {
	endpoint := fmt.Sprintf("/api/sessions/%s/mcp/prompts/%s/execute", sessionID, promptName)
	var result struct {
		Result string `json:"result"`
	}
	err := c.doRequest(ctx, http.MethodPost, endpoint, args, &result)
	return result.Result, err
}

// Health checks the health of the remote server.
func (c *Client) Health(ctx context.Context) error {
	var resp api.HealthResponse
	return c.doRequest(ctx, http.MethodGet, "/health", nil, &resp)
}

// Ready checks if the remote server is ready to handle requests.
func (c *Client) Ready(ctx context.Context) (*api.ReadyResponse, error) {
	var resp api.ReadyResponse
	if err := c.doRequest(ctx, http.MethodGet, "/ready", nil, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// GetSessionRecoveryData retrieves recovery data for a session in case of store failure
func (c *Client) GetSessionRecoveryData(ctx context.Context, sessionID string) (map[string]any, error) {
	var data map[string]any
	endpoint := fmt.Sprintf("/api/sessions/%s/recovery", sessionID)
	err := c.doRequest(ctx, http.MethodGet, endpoint, nil, &data)
	return data, err
}

// BatchDeleteSessions deletes multiple sessions in a single operation
func (c *Client) BatchDeleteSessions(ctx context.Context, sessionIDs []string) (map[string]any, error) {
	var resp map[string]any
	req := api.BatchDeleteSessionsRequest{SessionIDs: sessionIDs}
	err := c.doRequest(ctx, http.MethodPost, "/api/sessions/batch/delete", req, &resp)
	return resp, err
}

// BatchExportSessions exports multiple sessions
func (c *Client) BatchExportSessions(ctx context.Context, sessionIDs []string, format string) (map[string]any, error) {
	var resp map[string]any
	req := api.BatchExportSessionsRequest{SessionIDs: sessionIDs, Format: format}
	err := c.doRequest(ctx, http.MethodPost, "/api/sessions/batch/export", req, &resp)
	return resp, err
}

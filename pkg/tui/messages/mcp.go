package messages

import "github.com/docker/docker-agent/pkg/runtime"

// MCPPromptMsg executes an MCP prompt with arguments.
type MCPPromptMsg struct {
	PromptName string
	Arguments  map[string]string
}

// ShowMCPPromptInputMsg shows input dialog for MCP prompt.
type ShowMCPPromptInputMsg struct {
	PromptName string
	PromptInfo any // mcptools.PromptInfo but avoiding import cycles
}

// InteractionResponseMsg routes one complete interaction response to its session.
type InteractionResponseMsg struct {
	SessionID string
	Response  runtime.InteractionResponse
}

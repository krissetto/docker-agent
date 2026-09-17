package messages

import "github.com/docker/docker-agent/pkg/runtime"

// Agent messages control agent switching, commands, and model selection.
type (
	// SwitchAgentMsg switches to a different agent.
	SwitchAgentMsg struct{ AgentName string }

	// ShowAgentDetailsMsg opens the read-only agent-details dialog for the
	// named agent (clicking the current agent's card or Ctrl+clicking any agent).
	ShowAgentDetailsMsg struct{ AgentName string }

	// AgentCommandMsg sends a command to the agent.
	AgentCommandMsg struct{ Command string }

	// OpenModelPickerMsg opens the model picker dialog.
	OpenModelPickerMsg struct{}

	// ModelPickerLoadedMsg carries choices discovered off the Update path.
	ModelPickerLoadedMsg struct {
		Models     []runtime.ModelChoice
		SessionID  string
		Generation uint64
		Err        error
	}

	// RefreshModelPickerMsg forces a refresh of model discovery and reopens
	// the model picker with the updated choices.
	RefreshModelPickerMsg struct{ Query string }

	// ModelPickerRefreshedMsg carries the asynchronously refreshed choices.
	ModelPickerRefreshedMsg struct {
		SessionID        string
		Generation       uint64
		Models           []runtime.ModelChoice
		Query            string
		Err              error
		CatalogRefreshed bool
	}

	// ChangeModelMsg changes the model for the current agent.
	ChangeModelMsg struct{ ModelRef string }

	// SetThinkingLevelMsg sets the reasoning-effort level of the current
	// agent's model (/effort command). An empty Level shows usage.
	SetThinkingLevelMsg struct{ Level string }

	// CycleThinkingLevelMsg is a projection-scoped footer action. The root
	// stamps RouteGeneration at emission; components never guess generations.
	CycleThinkingLevelMsg struct {
		SessionID         string
		AgentName         string
		ModelRef          string
		DisplayedModelRef string
		RouteGeneration   uint64
	}
)

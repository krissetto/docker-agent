package dialog

import (
	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/tui/components/toolconfirm"
	"github.com/docker/docker-agent/pkg/tui/messages"
)

// ToolRejectionDialogID is the unique identifier for the tool rejection reason dialog.
const ToolRejectionDialogID = "tool-rejection-reason"

// toolRejectionOptions adapts the shared preset rejection reasons to the
// multi-choice dialog options.
func toolRejectionOptions() []MultiChoiceOption {
	reasons := toolconfirm.RejectionReasons()
	options := make([]MultiChoiceOption, 0, len(reasons))
	for _, r := range reasons {
		options = append(options, MultiChoiceOption{ID: r.ID, Label: r.Label, Value: r.Value})
	}
	return options
}

// NewToolRejectionReasonDialog creates a multi-choice dialog for selecting
// the reason for rejecting a tool call.
func NewToolRejectionReasonDialog(sessionID, requestID string) Dialog {
	return NewMultiChoiceDialog(MultiChoiceConfig{
		DialogID:          ToolRejectionDialogID,
		Title:             "Why reject this tool call?",
		Options:           toolRejectionOptions(),
		AllowCustom:       true,
		AllowSecondary:    true,
		SecondaryLabel:    "Skip",
		PrimaryLabel:      "Reject",
		CustomPlaceholder: "Other reason...",
		Context: messages.InteractionResponseMsg{
			SessionID: sessionID,
			Response:  runtime.InteractionResponse{InteractionID: requestID, Kind: runtime.InteractionConfirmation},
		},
	})
}

// HandleToolRejectionResult processes the result from the tool rejection dialog
// and returns the InteractionResponseMsg answering the confirmation the dialog
// was opened for (its correlation travels in the dialog's Context).
// Returns nil if the result was cancelled (user should stay in confirmation dialog).
func HandleToolRejectionResult(result MultiChoiceResult, correlation messages.InteractionResponseMsg) *messages.InteractionResponseMsg {
	if result.IsCancelled {
		// User pressed Esc - don't send resume, let them stay in confirmation dialog
		return nil
	}

	// Build the reason string
	reason := result.Value
	if result.IsSkipped {
		reason = "" // No reason provided
	}

	response := correlation
	response.Response.Kind = runtime.InteractionConfirmation
	response.Response.Resume = toolconfirm.Reject.Resume("", reason)
	return &response
}

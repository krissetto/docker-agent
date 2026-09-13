package dialog

import (
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/runtime"
	"github.com/docker/docker-agent/pkg/tools"
	"github.com/docker/docker-agent/pkg/tui/messages"
)

func TestNewURLElicitationDialog(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		message string
		url     string
	}{
		{
			name:    "with message and URL",
			message: "Please authorize access",
			url:     "https://example.com/auth",
		},
		{
			name:    "with empty URL",
			message: "Confirmation required",
			url:     "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			dialog := NewURLElicitationDialog(t.Context(), tt.message, tt.url, ElicitationRef{})
			require.NotNil(t, dialog)

			ud, ok := dialog.(*URLElicitationDialog)
			require.True(t, ok)
			assert.Equal(t, tt.message, ud.message)
			assert.Equal(t, tt.url, ud.url)
		})
	}
}

func TestURLElicitationDialog_View(t *testing.T) {
	t.Parallel()

	dialog := NewURLElicitationDialog(t.Context(), "Please visit the URL", "https://example.com/callback", ElicitationRef{}).(*URLElicitationDialog)
	dialog.SetSize(100, 50)

	view := dialog.View()

	// Should contain key elements
	assert.Contains(t, view, "MCP Server Request")
	assert.Contains(t, view, "Please visit the URL")
	assert.Contains(t, view, "https://example.com/callback")
	assert.Contains(t, view, "confirm")
	assert.NotContains(t, view, "cancel")
	assert.Contains(t, view, "open") // New "open" key binding
}

func TestURLElicitationDialog_HasOpenKeyBinding(t *testing.T) {
	t.Parallel()

	dialog := NewURLElicitationDialog(t.Context(), "Test", "https://example.com", ElicitationRef{}).(*URLElicitationDialog)

	// Verify the openBrowser key binding exists and is configured correctly
	require.NotNil(t, dialog.openBrowser)
	assert.NotEmpty(t, dialog.openBrowser.Keys())
}

// TestElicitationDialogsAnswerWithSessionCorrelation pins the routing contract
// of MCP elicitations in the TUI: the response a dialog emits must carry the
// session and interaction that raised the request, or the response
// handler cannot deliver it and the tool call hangs.
func TestElicitationDialogsAnswerWithSessionCorrelation(t *testing.T) {
	t.Parallel()

	ref := ElicitationRefFor(runtime.ElicitationRequest("Please confirm", "url", nil, "https://example.com", "elicit-1", "", "sess-1", nil, "root").(*runtime.ElicitationRequestEvent))
	ref.RequestID = "interaction-7"

	t.Run("url dialog", func(t *testing.T) {
		t.Parallel()
		d := NewURLElicitationDialog(t.Context(), "Please confirm", "https://example.com", ref).(*URLElicitationDialog)
		_, cmd := d.Update(tea.KeyPressMsg{Code: 'y', Text: "y"})
		response := findElicitationResponse(t, cmd)
		assert.Equal(t, tools.ElicitationActionAccept, response.Response.Elicitation.Action)
		assert.Equal(t, "sess-1", response.SessionID)
		assert.Equal(t, "interaction-7", response.Response.InteractionID)
		assert.Equal(t, "elicit-1", response.Response.ElicitationID)
	})

	t.Run("form dialog", func(t *testing.T) {
		t.Parallel()
		d := NewElicitationDialog("Anything to add?", nil, nil, ref).(*ElicitationDialog)
		_, cmd := d.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
		response := findElicitationResponse(t, cmd)
		assert.Equal(t, "sess-1", response.SessionID)
		assert.Equal(t, "interaction-7", response.Response.InteractionID)
		assert.Equal(t, "elicit-1", response.Response.ElicitationID)
	})
}

// findElicitationResponse runs cmd (and nested batches/sequences) and returns
// the InteractionResponseMsg it produced.
func findElicitationResponse(t *testing.T, cmd tea.Cmd) messages.InteractionResponseMsg {
	t.Helper()
	for _, msg := range collectMsgs(cmd) {
		if response, ok := msg.(messages.InteractionResponseMsg); ok {
			return response
		}
	}
	t.Fatal("dialog did not emit an InteractionResponseMsg")
	return messages.InteractionResponseMsg{}
}

package dialog

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/rivo/uniseg"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/widgets/textinput"
)

func TestParseElicitationSchema(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		schema         any
		expectedFields []ElicitationField
	}{
		{
			name:           "nil schema",
			schema:         nil,
			expectedFields: nil,
		},
		{
			name:           "empty schema",
			schema:         map[string]any{},
			expectedFields: nil,
		},
		{
			name: "schema with string property",
			schema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"username": map[string]any{
						"type":        "string",
						"description": "Your username",
					},
				},
			},
			expectedFields: []ElicitationField{
				{
					Name:        "username",
					Type:        "string",
					Description: "Your username",
					Required:    false,
				},
			},
		},
		{
			name: "schema with required fields",
			schema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"email": map[string]any{
						"type":        "string",
						"description": "Your email address",
					},
					"name": map[string]any{
						"type":        "string",
						"description": "Your name",
					},
				},
				"required": []any{"email"},
			},
			expectedFields: []ElicitationField{
				{
					Name:        "email",
					Type:        "string",
					Description: "Your email address",
					Required:    true,
				},
				{
					Name:        "name",
					Type:        "string",
					Description: "Your name",
					Required:    false,
				},
			},
		},
		{
			name: "schema with boolean property",
			schema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"remember_me": map[string]any{
						"type":        "boolean",
						"description": "Remember this device",
						"default":     true,
					},
				},
			},
			expectedFields: []ElicitationField{
				{
					Name:        "remember_me",
					Type:        "boolean",
					Description: "Remember this device",
					Required:    false,
					Default:     true,
				},
			},
		},
		{
			name: "schema with number property",
			schema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"count": map[string]any{
						"type":        "integer",
						"description": "Number of items",
						"default":     float64(10),
					},
				},
			},
			expectedFields: []ElicitationField{
				{
					Name:        "count",
					Type:        "integer",
					Description: "Number of items",
					Required:    false,
					Default:     float64(10),
				},
			},
		},
		{
			name: "schema with enum property",
			schema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"color": map[string]any{
						"type":        "string",
						"description": "Choose a color",
						"enum":        []any{"red", "green", "blue"},
					},
				},
			},
			expectedFields: []ElicitationField{
				{
					Name:        "color",
					Type:        "enum",
					Description: "Choose a color",
					Required:    false,
					EnumValues:  []string{"red", "green", "blue"},
				},
			},
		},
		{
			name: "schema with multiple properties sorted",
			schema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"zebra": map[string]any{
						"type": "string",
					},
					"apple": map[string]any{
						"type": "string",
					},
					"required_field": map[string]any{
						"type": "string",
					},
				},
				"required": []any{"required_field"},
			},
			expectedFields: []ElicitationField{
				{
					Name:     "required_field",
					Type:     "string",
					Required: true,
				},
				{
					Name:     "apple",
					Type:     "string",
					Required: false,
				},
				{
					Name:     "zebra",
					Type:     "string",
					Required: false,
				},
			},
		},
		{
			name: "schema with property title used for display",
			schema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"user_email": map[string]any{
						"type":        "string",
						"title":       "Email Address",
						"description": "Your email",
					},
				},
			},
			expectedFields: []ElicitationField{
				{
					Name:        "user_email",
					Title:       "Email Address",
					Type:        "string",
					Description: "Your email",
					Required:    false,
				},
			},
		},
		// Primitive schema tests
		{
			name: "primitive string schema with title",
			schema: map[string]any{
				"type":        "string",
				"title":       "Display Name",
				"description": "Your display name",
				"minLength":   float64(3),
				"maxLength":   float64(50),
				"default":     "user@example.com",
			},
			expectedFields: []ElicitationField{
				{
					Name:        "Display Name",
					Title:       "Display Name",
					Type:        "string",
					Description: "Your display name",
					Required:    true, // primitive schemas are implicitly required
					Default:     "user@example.com",
					MinLength:   3,
					MaxLength:   50,
				},
			},
		},
		{
			name: "primitive string schema without title",
			schema: map[string]any{
				"type":        "string",
				"description": "Enter a value",
			},
			expectedFields: []ElicitationField{
				{
					Name:        "value", // fallback name
					Type:        "string",
					Description: "Enter a value",
					Required:    true,
				},
			},
		},
		{
			name: "primitive boolean schema",
			schema: map[string]any{
				"type":    "boolean",
				"title":   "Accept Terms",
				"default": true,
			},
			expectedFields: []ElicitationField{
				{
					Name:     "Accept Terms",
					Title:    "Accept Terms",
					Type:     "boolean",
					Required: true,
					Default:  true,
				},
			},
		},
		{
			name: "primitive integer schema",
			schema: map[string]any{
				"type":    "integer",
				"title":   "Age",
				"default": float64(25),
			},
			expectedFields: []ElicitationField{
				{
					Name:     "Age",
					Title:    "Age",
					Type:     "integer",
					Required: true,
					Default:  float64(25),
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fields := parseElicitationSchema(tt.schema)
			assert.Equal(t, tt.expectedFields, fields)
		})
	}
}

func TestNewElicitationDialog(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name             string
		message          string
		schema           any
		meta             map[string]any
		expectedTitle    string
		hasFreeFormInput bool
	}{
		{
			name:             "simple dialog without fields has response input",
			message:          "Please confirm this action",
			schema:           nil,
			expectedTitle:    "Question",
			hasFreeFormInput: true,
		},
		{
			name:    "dialog with form fields",
			message: "Please enter your credentials",
			schema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"username": map[string]any{"type": "string", "description": "Your username"},
					"password": map[string]any{"type": "string", "description": "Your password"},
				},
				"required": []any{"username", "password"},
			},
			expectedTitle:    "Question",
			hasFreeFormInput: false,
		},
		{
			name:             "dialog with custom title from meta",
			message:          "Choose wisely",
			schema:           nil,
			meta:             map[string]any{"cagent/title": "Custom Title"},
			expectedTitle:    "Custom Title",
			hasFreeFormInput: true,
		},
		{
			name:             "dialog with empty meta defaults to Question",
			message:          "What?",
			schema:           nil,
			meta:             map[string]any{},
			expectedTitle:    "Question",
			hasFreeFormInput: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			dialog := NewElicitationDialog(tt.message, tt.schema, tt.meta, ElicitationRef{})
			require.NotNil(t, dialog)

			ed, ok := dialog.(*ElicitationDialog)
			require.True(t, ok)
			assert.Equal(t, tt.message, ed.message)
			assert.Equal(t, tt.expectedTitle, ed.title)
			assert.Equal(t, tt.hasFreeFormInput, ed.hasFreeFormInput())
		})
	}
}

func TestElicitationDialog_collectAndValidate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		schema        any
		setupInputs   func(*ElicitationDialog)
		expectedValid bool
		expectedKeys  []string
	}{
		{
			name:          "no fields",
			schema:        nil,
			expectedValid: true,
		},
		{
			name: "required field empty",
			schema: map[string]any{
				"type":       "object",
				"properties": map[string]any{"name": map[string]any{"type": "string"}},
				"required":   []any{"name"},
			},
			expectedValid: false,
		},
		{
			name: "required field filled",
			schema: map[string]any{
				"type":       "object",
				"properties": map[string]any{"name": map[string]any{"type": "string"}},
				"required":   []any{"name"},
			},
			setupInputs:   func(d *ElicitationDialog) { d.inputs[0].SetValue("test_name") },
			expectedValid: true,
			expectedKeys:  []string{"name"},
		},
		{
			name: "boolean field",
			schema: map[string]any{
				"type":       "object",
				"properties": map[string]any{"enabled": map[string]any{"type": "boolean"}},
			},
			setupInputs:   func(d *ElicitationDialog) { d.boolValues[0] = true },
			expectedValid: true,
			expectedKeys:  []string{"enabled"},
		},
		{
			name: "invalid integer",
			schema: map[string]any{
				"type":       "object",
				"properties": map[string]any{"count": map[string]any{"type": "integer"}},
				"required":   []any{"count"},
			},
			setupInputs:   func(d *ElicitationDialog) { d.inputs[0].SetValue("not_a_number") },
			expectedValid: false,
		},
		{
			name: "valid integer",
			schema: map[string]any{
				"type":       "object",
				"properties": map[string]any{"count": map[string]any{"type": "integer"}},
				"required":   []any{"count"},
			},
			setupInputs:   func(d *ElicitationDialog) { d.inputs[0].SetValue("42") },
			expectedValid: true,
			expectedKeys:  []string{"count"},
		},
		{
			name: "valid enum value",
			schema: map[string]any{
				"type":       "object",
				"properties": map[string]any{"color": map[string]any{"type": "string", "enum": []any{"red", "green", "blue"}}},
				"required":   []any{"color"},
			},
			setupInputs:   func(d *ElicitationDialog) { d.enumIndexes[0] = 0 }, // select "red"
			expectedValid: true,
			expectedKeys:  []string{"color"},
		},
		{
			name: "minLength validation fails",
			schema: map[string]any{
				"type":      "string",
				"title":     "Name",
				"minLength": float64(5),
			},
			setupInputs:   func(d *ElicitationDialog) { d.inputs[0].SetValue("abc") }, // 3 chars, need 5
			expectedValid: false,
		},
		{
			name: "minLength validation passes",
			schema: map[string]any{
				"type":      "string",
				"title":     "Name",
				"minLength": float64(3),
			},
			setupInputs:   func(d *ElicitationDialog) { d.inputs[0].SetValue("abcde") },
			expectedValid: true,
			expectedKeys:  []string{"Name"},
		},
		{
			name: "email format validation fails",
			schema: map[string]any{
				"type":   "string",
				"title":  "Email",
				"format": "email",
			},
			setupInputs:   func(d *ElicitationDialog) { d.inputs[0].SetValue("not-an-email") },
			expectedValid: false,
		},
		{
			name: "email format validation passes",
			schema: map[string]any{
				"type":   "string",
				"title":  "Email",
				"format": "email",
			},
			setupInputs:   func(d *ElicitationDialog) { d.inputs[0].SetValue("test@example.com") },
			expectedValid: true,
			expectedKeys:  []string{"Email"},
		},
		{
			name: "uri format validation fails",
			schema: map[string]any{
				"type":   "string",
				"title":  "Website",
				"format": "uri",
			},
			setupInputs:   func(d *ElicitationDialog) { d.inputs[0].SetValue("not-a-url") },
			expectedValid: false,
		},
		{
			name: "uri format validation passes",
			schema: map[string]any{
				"type":   "string",
				"title":  "Website",
				"format": "uri",
			},
			setupInputs:   func(d *ElicitationDialog) { d.inputs[0].SetValue("https://example.com") },
			expectedValid: true,
			expectedKeys:  []string{"Website"},
		},
		{
			name: "date format validation passes",
			schema: map[string]any{
				"type":   "string",
				"title":  "Birthday",
				"format": "date",
			},
			setupInputs:   func(d *ElicitationDialog) { d.inputs[0].SetValue("2024-01-15") },
			expectedValid: true,
			expectedKeys:  []string{"Birthday"},
		},
		{
			name: "pattern validation fails",
			schema: map[string]any{
				"type":    "string",
				"title":   "Code",
				"pattern": "^[A-Z]{3}$",
			},
			setupInputs:   func(d *ElicitationDialog) { d.inputs[0].SetValue("abc") },
			expectedValid: false,
		},
		{
			name: "pattern validation passes",
			schema: map[string]any{
				"type":    "string",
				"title":   "Code",
				"pattern": "^[A-Z]{3}$",
			},
			setupInputs:   func(d *ElicitationDialog) { d.inputs[0].SetValue("ABC") },
			expectedValid: true,
			expectedKeys:  []string{"Code"},
		},
		{
			name: "number minimum validation fails",
			schema: map[string]any{
				"type":    "number",
				"title":   "Age",
				"minimum": float64(18),
			},
			setupInputs:   func(d *ElicitationDialog) { d.inputs[0].SetValue("15") },
			expectedValid: false,
		},
		{
			name: "number minimum validation passes",
			schema: map[string]any{
				"type":    "number",
				"title":   "Age",
				"minimum": float64(18),
			},
			setupInputs:   func(d *ElicitationDialog) { d.inputs[0].SetValue("25") },
			expectedValid: true,
			expectedKeys:  []string{"Age"},
		},
		{
			name: "number maximum validation fails",
			schema: map[string]any{
				"type":    "integer",
				"title":   "Count",
				"maximum": float64(100),
			},
			setupInputs:   func(d *ElicitationDialog) { d.inputs[0].SetValue("150") },
			expectedValid: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			dialog := NewElicitationDialog("test", tt.schema, nil, ElicitationRef{}).(*ElicitationDialog)
			if tt.setupInputs != nil {
				tt.setupInputs(dialog)
			}

			content, firstErrorIdx := dialog.collectAndValidate()
			valid := firstErrorIdx < 0
			assert.Equal(t, tt.expectedValid, valid)

			if valid && tt.expectedKeys != nil {
				for _, key := range tt.expectedKeys {
					assert.Contains(t, content, key)
				}
			}
		})
	}
}

func TestElicitationDialog_PasswordFieldMaskedAndUntrimmed(t *testing.T) {
	t.Parallel()

	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"password": map[string]any{"type": "string", "format": "password"},
		},
		"required": []any{"password"},
	}
	dialog := NewElicitationDialog("sudo", schema, nil, ElicitationRef{}).(*ElicitationDialog)

	// The password input is masked, not echoed in clear text.
	assert.Equal(t, textinput.EchoPassword, dialog.inputs[0].EchoMode)

	// A password with surrounding whitespace must round-trip unchanged
	// (regular string fields are trimmed, secret ones are not).
	dialog.inputs[0].SetValue("  s p a c e d  ")
	content, firstErrorIdx := dialog.collectAndValidate()
	assert.Equal(t, -1, firstErrorIdx)
	assert.Equal(t, "  s p a c e d  ", content["password"])
}

func TestElicitationDialog_LongEnumScrolls(t *testing.T) {
	t.Parallel()

	// Build an enum with many values that would overflow a typical terminal.
	enumValues := make([]any, 0, 30)
	for i := range 30 {
		enumValues = append(enumValues, "option-"+strings.Repeat("x", 5)+string(rune('A'+i%26)))
	}

	schema := map[string]any{
		"type":  "string",
		"title": "Pick one",
		"enum":  enumValues,
	}

	dialog := NewElicitationDialog("Choose an option:", schema, nil, ElicitationRef{}).(*ElicitationDialog)
	require.Len(t, dialog.fields, 1)
	require.Len(t, dialog.fields[0].EnumValues, 30)

	_, _ = dialog.Update(tea.WindowSizeMsg{Width: 100, Height: 20})
	_ = dialog.View() // populate fieldStarts and configure the scrollview

	require.Len(t, dialog.fieldStarts, 1)
	assert.True(t, dialog.scrollview.NeedsScrollbar(), "30 enum options on a 20-row terminal must require scrolling")

	// Selecting an option far down the list must auto-scroll it into view.
	dialog.enumIndexes[0] = 25
	dialog.ensureFocusVisible()
	_ = dialog.View()
	offset := dialog.scrollview.ScrollOffset()
	visEnd := offset + dialog.scrollview.VisibleHeight() - 1
	selectedLine := dialog.fieldStarts[0] + 1 + 25
	assert.GreaterOrEqual(t, selectedLine, offset, "selected option must be at or below scroll offset")
	assert.LessOrEqual(t, selectedLine, visEnd, "selected option must be at or above visible end")
}

func TestElicitationDialog_FieldsBelowFold_AreReachable(t *testing.T) {
	t.Parallel()

	props := map[string]any{}
	required := []any{}
	for i := range 15 {
		name := "field" + string(rune('a'+i%26)) + string(rune('0'+i/26))
		props[name] = map[string]any{"type": "string", "title": name}
		required = append(required, name)
	}
	schema := map[string]any{"type": "object", "properties": props, "required": required}

	dialog := NewElicitationDialog("Fill in the form", schema, nil, ElicitationRef{}).(*ElicitationDialog)
	_, _ = dialog.Update(tea.WindowSizeMsg{Width: 100, Height: 18})
	_ = dialog.View()

	require.Len(t, dialog.fieldStarts, 15)
	assert.True(t, dialog.scrollview.NeedsScrollbar(), "15 fields on an 18-row terminal must require scrolling")

	// Tab through every field; each one must end up within the scroll viewport.
	for i := range 15 {
		for dialog.currentField != i {
			dialog.moveFocus(1)
		}
		_ = dialog.View()
		offset := dialog.scrollview.ScrollOffset()
		visEnd := offset + dialog.scrollview.VisibleHeight() - 1
		start := dialog.fieldStarts[i]
		assert.GreaterOrEqual(t, start, offset, "field %d label must be at or below scroll offset", i)
		assert.LessOrEqual(t, start, visEnd, "field %d label must be visible", i)
	}
}

func TestElicitationDialog_SmallContent_NoScrollbar(t *testing.T) {
	t.Parallel()

	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"name": map[string]any{"type": "string", "title": "Name"},
		},
		"required": []any{"name"},
	}

	dialog := NewElicitationDialog("Enter your name", schema, nil, ElicitationRef{}).(*ElicitationDialog)
	_, _ = dialog.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	_ = dialog.View()

	assert.False(t, dialog.scrollview.NeedsScrollbar(), "small dialogs should not need a scrollbar")
}

// TestElicitationDialog_TypingRevealsBelowFoldField pins that initial focus
// is visible, manual scrolling can reveal the question without snapping back,
// and typing subsequently reveals the focused input again.
func TestElicitationDialog_TypingRevealsBelowFoldField(t *testing.T) {
	t.Parallel()

	longMessage := strings.Repeat("Long question line. ", 30)
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"name": map[string]any{"type": "string", "title": "Name"},
		},
		"required": []any{"name"},
	}

	dialog := NewElicitationDialog(longMessage, schema, nil, ElicitationRef{}).(*ElicitationDialog)
	_, _ = dialog.Update(tea.WindowSizeMsg{Width: 80, Height: 16})
	_ = dialog.View()

	require.True(t, dialog.scrollview.NeedsScrollbar(), "long message + field must require scrolling")
	require.Len(t, dialog.fieldStarts, 1)
	inputLine := dialog.fieldStarts[0] + dialog.fieldGeoms[0].labelHeight
	require.Positive(t, dialog.BodyScrollOffset(), "initial preparation reveals the focused input below the question")
	require.GreaterOrEqual(t, inputLine, dialog.BodyScrollOffset())
	require.Less(t, inputLine, dialog.BodyScrollOffset()+dialog.scrollview.VisibleHeight())
	require.Contains(t, ansi.Strip(dialog.View()), "Enter value")

	// Deliberate scrolling to the question survives ordinary paint/preparation.
	dialog.scrollview.ScrollToTop()
	dialog.SetSize(80, 16)
	dialog.View()
	require.Zero(t, dialog.BodyScrollOffset(), "manual scroll remains until an input/focus event")
	// The text field's input line lives below that manually selected viewport.
	require.Greater(t, inputLine, dialog.scrollview.VisibleHeight()-1,
		"test setup: the text field must initially be below the fold")

	// User types a character — this must scroll the input into view.
	_, _ = dialog.Update(tea.KeyPressMsg(tea.Key{Code: 'a', Text: "a"}))
	_ = dialog.View()

	offset := dialog.scrollview.ScrollOffset()
	visEnd := offset + dialog.scrollview.VisibleHeight() - 1
	assert.GreaterOrEqual(t, inputLine, offset, "input line must be at or below scroll offset after typing")
	assert.LessOrEqual(t, inputLine, visEnd, "input line must be visible after typing")
}

func TestElicitationDialog_OpensWithFocusedOptionVisible(t *testing.T) {
	t.Parallel()

	// Long question that, combined with many options, forces a scrollbar.
	longMessage := strings.Repeat("This is a long question that takes several lines. ", 20)

	enumValues := make([]any, 0, 12)
	for i := range 12 {
		enumValues = append(enumValues, "option-"+string(rune('A'+i)))
	}

	schema := map[string]any{
		"type":  "string",
		"title": "Pick one",
		"enum":  enumValues,
	}

	dialog := NewElicitationDialog(longMessage, schema, nil, ElicitationRef{}).(*ElicitationDialog)
	_, _ = dialog.Update(tea.WindowSizeMsg{Width: 80, Height: 18})
	_ = dialog.View()

	require.True(t, dialog.scrollview.NeedsScrollbar(), "long question + many options must require scrolling")
	first, last := dialog.focusRange()
	require.Positive(t, dialog.BodyScrollOffset(), "initial preparation reveals the selected option")
	require.GreaterOrEqual(t, first, dialog.BodyScrollOffset())
	require.Less(t, last, dialog.BodyScrollOffset()+dialog.scrollview.VisibleHeight())
	require.Contains(t, ansi.Strip(dialog.View()), "option-A")
	dialog.scrollview.ScrollToTop()
	dialog.SetSize(80, 18)
	dialog.View()
	require.Zero(t, dialog.BodyScrollOffset(), "manual question reading is not overridden by paint or unchanged size")
	dialog.focusField(0)
	dialog.View()
	require.GreaterOrEqual(t, first, dialog.BodyScrollOffset())
	require.Less(t, last, dialog.BodyScrollOffset()+dialog.scrollview.VisibleHeight(), "explicit focus reveals the option again")
}

// TestElicitationDialog_UserScrollUp_NotSnappedBack pins the contract that
// once the user scrolls up (e.g. via wheel/PgUp) to read the question, the
// next render must not snap the viewport back down to the focused option.
func TestElicitationDialog_UserScrollUp_NotSnappedBack(t *testing.T) {
	t.Parallel()

	longMessage := strings.Repeat("Long question line. ", 30)
	enumValues := make([]any, 0, 12)
	for i := range 12 {
		enumValues = append(enumValues, "option-"+string(rune('A'+i)))
	}
	schema := map[string]any{"type": "string", "title": "Pick one", "enum": enumValues}

	dialog := NewElicitationDialog(longMessage, schema, nil, ElicitationRef{}).(*ElicitationDialog)
	_, _ = dialog.Update(tea.WindowSizeMsg{Width: 80, Height: 16})
	_ = dialog.View()

	require.True(t, dialog.scrollview.NeedsScrollbar())

	// Scroll all the way down (e.g. user pressed End).
	dialog.scrollview.ScrollToBottom()
	offsetAfterScroll := dialog.scrollview.ScrollOffset()
	require.Positive(t, offsetAfterScroll, "scrollview should accept a non-zero offset")

	// Re-render: must NOT snap back to keep the focused option visible.
	_ = dialog.View()
	assert.Equal(t, offsetAfterScroll, dialog.scrollview.ScrollOffset(),
		"re-rendering must not auto-scroll; the user's scroll position must be preserved")

	// Now scroll back to the top (to re-read the question).
	dialog.scrollview.ScrollToTop()
	_ = dialog.View()
	assert.Equal(t, 0, dialog.scrollview.ScrollOffset(),
		"re-rendering must not auto-scroll back down to the focused option")
}

// TestElicitationDialog_ResizeReanchorsFocus pins that a window resize —
// which rewraps long field titles and shifts the focused field far from the
// old scroll offset — brings the focused control back into view on the next
// render, and that this reanchor is one-shot: later ordinary renders must
// not override a deliberate user scroll.
func TestElicitationDialog_ResizeReanchorsFocus(t *testing.T) {
	t.Parallel()

	props := map[string]any{}
	for i := range 6 {
		name := "field" + string(rune('a'+i))
		props[name] = map[string]any{
			"type":  "string",
			"title": "Field " + string(rune('A'+i)) + ": " + strings.Repeat("very long title ", 3),
		}
	}
	schema := map[string]any{"type": "object", "properties": props}

	dialog := NewElicitationDialog("Fill in:", schema, nil, ElicitationRef{}).(*ElicitationDialog)
	_, _ = dialog.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	_ = dialog.View()

	// Focus the last field; the wide layout scrolls it into view.
	dialog.focusField(len(dialog.fields) - 1)
	_ = dialog.View()
	require.Equal(t, 1, dialog.fieldGeoms[dialog.currentField].labelHeight,
		"test setup: titles must fit on one row at 120 columns")
	staleOffset := dialog.scrollview.ScrollOffset()

	// Shrink to 20 columns: every title above the focused field now wraps
	// over several rows, pushing the focused input far below the old offset.
	_, _ = dialog.Update(tea.WindowSizeMsg{Width: 20, Height: 30})
	_ = dialog.View()

	require.Greater(t, dialog.fieldGeoms[dialog.currentField].labelHeight, 1,
		"test setup: titles must wrap at 20 columns")
	first, last := dialog.focusRange()
	require.GreaterOrEqual(t, first, 0)
	require.Greater(t, first, staleOffset+dialog.scrollview.VisibleHeight()-1,
		"test setup: rewrapping must push the focused field below the pre-resize viewport")

	offset := dialog.scrollview.ScrollOffset()
	visEnd := offset + dialog.scrollview.VisibleHeight() - 1
	assert.GreaterOrEqual(t, first, offset, "focused input must be at or below the scroll offset after resize")
	assert.LessOrEqual(t, last, visEnd, "focused input must be visible after resize")

	// The reanchor is one-shot: a deliberate scroll away from the focused
	// field must survive subsequent ordinary renders.
	dialog.scrollview.ScrollToTop()
	_ = dialog.View()
	assert.Equal(t, 0, dialog.scrollview.ScrollOffset(),
		"ordinary re-render after the resize reanchor must not auto-scroll back to the focused field")
}

// TestElicitationDialog_ResizeFreeFormInput_ReanchorsOnce pins that initial
// preparation and resizing reveal the free-form cursor, while subsequent
// manual scrolling and ordinary renders do not snap back to it.
func TestElicitationDialog_ResizeFreeFormInput_ReanchorsOnce(t *testing.T) {
	t.Parallel()

	longMessage := strings.Repeat("Long question line. ", 40)
	dialog := NewElicitationDialog(longMessage, nil, nil, ElicitationRef{}).(*ElicitationDialog)
	for _, size := range [][2]int{{120, 16}, {20, 16}} {
		_, _ = dialog.Update(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
		require.True(t, dialog.scrollview.NeedsScrollbar())
		inputLine := len(dialog.layout().bodyLines) - 1
		require.Positive(t, dialog.BodyScrollOffset())
		require.GreaterOrEqual(t, inputLine, dialog.BodyScrollOffset())
		require.Less(t, inputLine, dialog.BodyScrollOffset()+dialog.scrollview.VisibleHeight())
		view := dialog.View()
		x, y, width, height := dialog.BodyScrollBounds()
		row, _ := dialog.Position()
		inputY := y + inputLine - dialog.BodyScrollOffset()
		lines := strings.Split(view, "\n")
		require.Contains(t, ansi.Strip(lines[inputY-row]), "Type your")
		require.Regexp(t, `\x1b\[[0-9;]*\b7(?:;[0-9;]*)?mT`, lines[inputY-row], "focused placeholder cursor is painted after resize")
		require.Positive(t, width)
		require.Less(t, inputY, y+height)
		_, _ = dialog.Update(tea.MouseClickMsg{Button: tea.MouseLeft, X: x, Y: inputY})
		require.True(t, dialog.responseInput.Focused(), "painted response row retains its input hit target")

		dialog.scrollview.ScrollToTop()
		dialog.SetSize(size[0], size[1])
		dialog.View()
		require.Zero(t, dialog.BodyScrollOffset(), "same-size preparation and rendering preserve manual scrolling")
	}
}

// squashText strips ANSI sequences and removes all whitespace, so wrapped
// output can be compared against its source text regardless of where the
// wrapping primitive breaks lines (word boundaries or mid-token).
func squashText(s string) string {
	return strings.Join(strings.Fields(ansi.Strip(s)), "")
}

// clickBodyLine simulates a left mouse click on the given line of the
// scrollable body, translated to absolute screen coordinates via the
// click-zone cache populated by View().
func clickBodyLine(t *testing.T, d *ElicitationDialog, line int) {
	t.Helper()
	require.NotZero(t, d.scrollableRow, "View() must run before simulating clicks")
	relY := line - d.scrollview.ScrollOffset()
	require.GreaterOrEqual(t, relY, 0, "line %d must be inside the viewport", line)
	require.Less(t, relY, d.scrollview.VisibleHeight(), "line %d must be inside the viewport", line)
	_, _ = d.Update(tea.MouseClickMsg{X: d.scrollableCol + 1, Y: d.scrollableRow + relY, Button: tea.MouseLeft})
}

// assertBodyTextFits asserts that no body line carries visible text wider
// than the scrollview's inner width — wider text would be silently clipped.
// Trailing padding spaces are ignored: the body is padded to its widest part
// (e.g. the textinput renders one cell wider than its set width) and the
// scrollview trims that padding back off without losing anything.
func assertBodyTextFits(t *testing.T, bodyLines []string, innerWidth int) {
	t.Helper()
	for i, line := range bodyLines {
		text := strings.TrimRight(ansi.Strip(line), " ")
		assert.LessOrEqual(t, ansi.StringWidth(text), innerWidth,
			"body line %d visible text must fit the scrollview width, got %q", i, text)
	}
}

// TestElicitationDialog_LongFieldTitleWraps pins that an agent-supplied field
// title wider than the dialog's content width wraps across multiple rows
// instead of being silently clipped by the scrollview.
func TestElicitationDialog_LongFieldTitleWraps(t *testing.T) {
	t.Parallel()

	longTitle := "Please choose the deployment strategy that best matches your current production rollout constraints and compliance requirements"
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"strategy": map[string]any{"type": "string", "title": longTitle},
		},
	}

	dialog := NewElicitationDialog("Question:", schema, nil, ElicitationRef{}).(*ElicitationDialog)
	_, _ = dialog.Update(tea.WindowSizeMsg{Width: 60, Height: 30})
	_ = dialog.View()

	l := dialog.layout()
	innerWidth := max(1, l.contentWidth-dialog.scrollview.ReservedCols())
	require.Greater(t, ansi.StringWidth(longTitle), innerWidth,
		"test setup: the title must be wider than the body width")

	require.Len(t, dialog.fieldGeoms, 1)
	assert.Greater(t, dialog.fieldGeoms[0].labelHeight, 1, "long title must wrap over several rows")

	assert.Contains(t, squashText(strings.Join(l.bodyLines, "\n")), squashText(longTitle),
		"the full title must survive wrapping")
	assertBodyTextFits(t, l.bodyLines, innerWidth)
}

// TestElicitationDialog_LongEnumOptionWrapsAndClicks pins that a long enum
// option — including an unbroken token wider than the dialog — wraps without
// losing text, and that every wrapped row still selects that option on click.
func TestElicitationDialog_LongEnumOptionWrapsAndClicks(t *testing.T) {
	t.Parallel()

	longOption := "retry with exponential backoff " + strings.Repeat("x", 80)
	schema := map[string]any{
		"type":  "string",
		"title": "Pick one",
		"enum":  []any{"deploy-canary", longOption, "rollback"},
	}

	dialog := NewElicitationDialog("Choose:", schema, nil, ElicitationRef{}).(*ElicitationDialog)
	_, _ = dialog.Update(tea.WindowSizeMsg{Width: 60, Height: 40})
	_ = dialog.View()

	require.Len(t, dialog.fieldGeoms, 1)
	geom := dialog.fieldGeoms[0]
	require.Len(t, geom.optionStarts, 3)
	require.Greater(t, geom.optionHeights[1], 1, "long option must wrap over several rows")

	l := dialog.layout()
	innerWidth := max(1, l.contentWidth-dialog.scrollview.ReservedCols())
	assert.Contains(t, squashText(strings.Join(l.bodyLines, "\n")), squashText(longOption),
		"the full option text, including the unbroken token, must survive wrapping")
	assertBodyTextFits(t, l.bodyLines, innerWidth)

	// Every wrapped row of the long option must select it on click.
	start := dialog.fieldStarts[0]
	for row := range geom.optionHeights[1] {
		dialog.enumIndexes[0] = 0
		clickBodyLine(t, dialog, start+geom.optionStarts[1]+row)
		assert.Equal(t, 1, dialog.enumIndexes[0], "click on wrapped row %d must select the long option", row)
	}

	// Options shifted down by the wrapping must still map correctly.
	clickBodyLine(t, dialog, start+geom.optionStarts[2])
	assert.Equal(t, 2, dialog.enumIndexes[0])

	// Label rows must not select any option.
	clickBodyLine(t, dialog, start)
	assert.Equal(t, 2, dialog.enumIndexes[0], "clicking the label must not change the selection")
}

// TestElicitationDialog_WrappedOptionFocusStaysVisible pins that
// ensureFocusVisible accounts for wrapped option rows: walking the selection
// through multi-row options in a small viewport keeps every row of the
// selected option on screen.
func TestElicitationDialog_WrappedOptionFocusStaysVisible(t *testing.T) {
	t.Parallel()

	longMessage := strings.Repeat("Long question line. ", 30)
	enumValues := make([]any, 0, 8)
	for i := range 8 {
		enumValues = append(enumValues, "option "+string(rune('A'+i))+" "+strings.Repeat("verbose descriptive choice text ", 3))
	}
	schema := map[string]any{"type": "string", "title": "Pick one", "enum": enumValues}

	dialog := NewElicitationDialog(longMessage, schema, nil, ElicitationRef{}).(*ElicitationDialog)
	_, _ = dialog.Update(tea.WindowSizeMsg{Width: 60, Height: 16})
	_ = dialog.View()

	require.True(t, dialog.scrollview.NeedsScrollbar(), "long message + wrapped options must require scrolling")
	require.Greater(t, dialog.fieldGeoms[0].optionHeights[0], 1, "test setup: options must wrap")

	// Walk the selection down through every option; each step must bring the
	// whole wrapped option into the viewport.
	for range len(enumValues) - 1 {
		dialog.moveSelection(1)
		_ = dialog.View()

		idx := dialog.enumIndexes[0]
		geom := dialog.fieldGeoms[0]
		first := dialog.fieldStarts[0] + geom.optionStarts[idx]
		last := first + geom.optionHeights[idx] - 1
		offset := dialog.scrollview.ScrollOffset()
		visEnd := offset + dialog.scrollview.VisibleHeight() - 1
		assert.GreaterOrEqual(t, first, offset, "option %d first row must be visible", idx)
		assert.LessOrEqual(t, last, visEnd, "option %d last row must be visible", idx)
	}
}

// TestElicitationDialog_ExplicitNewlinesShiftRows pins that explicit newlines
// in agent-supplied labels are preserved and that click geometry accounts for
// the extra rows: a boolean's Yes/No rows sit below the full multi-row label
// instead of being addressed by the old one-row-per-label offsets.
func TestElicitationDialog_ExplicitNewlinesShiftRows(t *testing.T) {
	t.Parallel()

	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"confirm": map[string]any{
				"type":  "boolean",
				"title": "Proceed with the change?\n(this cannot be undone)",
			},
		},
	}

	dialog := NewElicitationDialog("Careful:", schema, nil, ElicitationRef{}).(*ElicitationDialog)
	_, _ = dialog.Update(tea.WindowSizeMsg{Width: 80, Height: 30})
	_ = dialog.View()

	require.Len(t, dialog.fieldGeoms, 1)
	geom := dialog.fieldGeoms[0]
	require.Equal(t, 2, geom.labelHeight, "explicit newline must produce a second label row")
	require.Equal(t, []int{2, 3}, geom.optionStarts, "Yes/No rows must start below the full label")

	start := dialog.fieldStarts[0]

	// Row 2 is "Yes" (the old one-row-per-label math mapped it to "No").
	clickBodyLine(t, dialog, start+2)
	assert.True(t, dialog.boolValues[0], "row below the two-line label must be Yes")

	clickBodyLine(t, dialog, start+3)
	assert.False(t, dialog.boolValues[0], "next row must be No")

	// The label's second row must not toggle anything.
	clickBodyLine(t, dialog, start+1)
	assert.False(t, dialog.boolValues[0])
}

func TestFormTextCursorClickPreservesHorizontalViewport(t *testing.T) {
	for _, tc := range []struct {
		name, value string
		width       int
	}{
		{"repeated-ascii", strings.Repeat("a", 60), 9},
		{"cjk", strings.Repeat("界語", 20), 10},
		{"combining", strings.Repeat("e\u0301", 30), 10},
		{"mixed", "abcdef界語e\u0301" + strings.Repeat("文字xy", 12), 13},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := textinput.New()
			input.Prompt = ""
			input.SetWidth(tc.width)
			input.SetValue(tc.value)
			input.CursorEnd()
			input.Focus()
			// Move within an already-scrolled viewport: position alone cannot recover its offset.
			input.SetCursor(max(0, input.Position()-2))
			before := ansi.Strip(input.View())
			stylesBefore := input.Styles()
			probe := input
			SetTextInputCursorAtCell(&probe, 0)
			offset := probe.Position()
			require.Positive(t, offset, "long input must remain scrolled, not jump to absolute start")
			visible := []rune(input.Value())[offset:]
			require.True(t, strings.HasPrefix(before, string(visible[:min(2, len(visible))])), "inferred offset must match the actual rendered prefix")
			g := uniseg.NewGraphemes(string(visible))
			require.True(t, g.Next())
			firstWidth, firstRunes := g.Width(), len(g.Runes())
			if firstWidth > 1 {
				probe = input
				SetTextInputCursorAtCell(&probe, 1)
				assert.Equal(t, offset, probe.Position(), "both cells of a wide glyph target its start")
			}
			probe = input
			SetTextInputCursorAtCell(&probe, firstWidth)
			assert.Equal(t, offset+firstRunes, probe.Position(), "combining marks stay with their grapheme")
			assert.Equal(t, before, ansi.Strip(input.View()), "probing a copy leaves the input viewport intact")
			assert.Equal(t, stylesBefore, input.Styles(), "probing never replaces actual styles")
		})
	}
}

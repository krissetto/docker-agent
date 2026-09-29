package tui

import (
	"github.com/docker/docker-agent/pkg/tui/widgets/key"
	"charm.land/bubbles/v2/textarea"
	"charm.land/bubbles/v2/textinput"

	"github.com/docker/docker-agent/pkg/tui/help"
)

// inputHelp uses the inherited canonical maps. Current is filtered by root
// routing; Reference labels the host and warns about intercepted aliases.
func inputHelp(host string, multiline bool) help.Section {
	type inputAction struct {
		id, description string
		binding         key.Binding
	}
	var actions []inputAction
	if multiline {
		k := textarea.DefaultKeyMap()
		actions = []inputAction{
			{"CharacterForward", "character forward", k.CharacterForward},
			{"CharacterBackward", "character backward", k.CharacterBackward},
			{"WordForward", "word forward", k.WordForward},
			{"WordBackward", "word backward", k.WordBackward},
			{"LineNext", "line next", k.LineNext},
			{"LinePrevious", "line previous", k.LinePrevious},
			{"DeleteWordBackward", "delete word backward", k.DeleteWordBackward},
			{"DeleteWordForward", "delete word forward", k.DeleteWordForward},
			{"DeleteAfterCursor", "delete after cursor", k.DeleteAfterCursor},
			{"DeleteBeforeCursor", "delete before cursor", k.DeleteBeforeCursor},
			{"DeleteCharacterBackward", "delete character backward", k.DeleteCharacterBackward},
			{"DeleteCharacterForward", "delete character forward", k.DeleteCharacterForward},
			{"LineStart", "line start", k.LineStart},
			{"LineEnd", "line end", k.LineEnd},
			{"PageUp", "page up", k.PageUp},
			{"PageDown", "page down", k.PageDown},
			{"Paste", "paste", k.Paste},
			{"InputBegin", "input begin", k.InputBegin},
			{"InputEnd", "input end", k.InputEnd},
			{"CapitalizeWordForward", "capitalize word forward", k.CapitalizeWordForward},
			{"LowercaseWordForward", "lowercase word forward", k.LowercaseWordForward},
			{"UppercaseWordForward", "uppercase word forward", k.UppercaseWordForward},
			{"TransposeCharacterBackward", "transpose character backward", k.TransposeCharacterBackward},
			{"SelectCharacterForward", "select character forward", k.SelectCharacterForward},
			{"SelectCharacterBackward", "select character backward", k.SelectCharacterBackward},
			{"SelectWordForward", "select word forward", k.SelectWordForward},
			{"SelectWordBackward", "select word backward", k.SelectWordBackward},
			{"SelectLineUp", "select line up", k.SelectLineUp},
			{"SelectLineDown", "select line down", k.SelectLineDown},
			{"SelectAll", "select all", k.SelectAll},
			{"CopySelection", "copy selection", k.CopySelection},
		}
	} else {
		k := textinput.DefaultKeyMap()
		actions = []inputAction{
			{"CharacterForward", "character forward", k.CharacterForward},
			{"CharacterBackward", "character backward", k.CharacterBackward},
			{"WordForward", "word forward", k.WordForward},
			{"WordBackward", "word backward", k.WordBackward},
			{"DeleteWordBackward", "delete word backward", k.DeleteWordBackward},
			{"DeleteWordForward", "delete word forward", k.DeleteWordForward},
			{"DeleteAfterCursor", "delete after cursor", k.DeleteAfterCursor},
			{"DeleteBeforeCursor", "delete before cursor", k.DeleteBeforeCursor},
			{"DeleteCharacterBackward", "delete character backward", k.DeleteCharacterBackward},
			{"DeleteCharacterForward", "delete character forward", k.DeleteCharacterForward},
			{"LineStart", "line start", k.LineStart},
			{"LineEnd", "line end", k.LineEnd},
			{"Paste", "paste", k.Paste},
		}
	}
	title := map[string]string{"composer": "Composer", "inline": "Inline message", "title": "Sidebar title", "search": "History search"}[host]
	section := help.Section{ID: "input." + host, Title: title + " — text editing"}
	for _, action := range actions {
		entry := bindingHelp("input."+host+"."+action.id, action.binding, action.description)
		entry.Condition = title + " input; earlier controls in Current take precedence over these reference aliases"
		section.Entries = append(section.Entries, entry)
	}
	section.Entries = append(section.Entries, help.Entry{ID: "input." + host + ".text", Keys: []string{"printable text", "bracketed paste"}, Description: "Insert text", Condition: title + " input"})
	return section
}

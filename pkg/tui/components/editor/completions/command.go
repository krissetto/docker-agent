package completions

import (
	"slices"
	"strings"

	"github.com/docker/docker-agent/pkg/tui/commands"
	"github.com/docker/docker-agent/pkg/tui/components/completion"
)

type commandCompletion struct {
	categories []commands.Category
}

func NewCommandCompletion(categories []commands.Category) Completion {
	return &commandCompletion{
		categories: categories,
	}
}

func (c *commandCompletion) RequiresEmptyEditor() bool {
	return true
}

func (c *commandCompletion) Trigger() string {
	return "/"
}

func (c *commandCompletion) Items() []completion.Item {
	var items []completion.Item

	for _, cmd := range c.categories {
		for _, command := range cmd.Commands {
			items = append(items, completion.Item{
				Label:       command.Label,
				Description: command.Description,
				Value:       command.SlashCommand,
			})
		}
	}

	return sortItemsByLabel(items)
}

func sortItemsByLabel(items []completion.Item) []completion.Item {
	slices.SortFunc(items, func(a, b completion.Item) int {
		return strings.Compare(strings.ToLower(a.Label), strings.ToLower(b.Label))
	})
	return items
}

func (c *commandCompletion) MatchMode() completion.MatchMode {
	return completion.MatchPrefix
}

// ArgumentItems implements ArgumentCompleter: it looks up the command whose
// SlashCommand is the first token of line and, if it exposes a
// CompleteArgument or CompleteArgumentFor provider, maps its candidates to
// completion items. Contextual providers own filtering and relevance order;
// their items are pinned so concise labels are not filtered a second time
// against the complete multi-argument input.
// The full command line ("/cmd value") is used as Value so selecting an item
// replaces the editor content wholesale — this tolerates argument values
// containing spaces, which a word-based splice would not.
func (c *commandCompletion) ArgumentItems(line string) ([]completion.Item, bool) {
	slashCommand, argument, _ := strings.Cut(line, " ")

	for _, cat := range c.categories {
		for _, cmd := range cat.Commands {
			if cmd.SlashCommand != slashCommand || (cmd.CompleteArgument == nil && cmd.CompleteArgumentFor == nil) {
				continue
			}

			var candidates []commands.ArgumentCandidate
			if cmd.CompleteArgumentFor != nil {
				candidates = cmd.CompleteArgumentFor(argument)
			} else {
				candidates = cmd.CompleteArgument()
			}
			items := make([]completion.Item, 0, len(candidates))
			for _, candidate := range candidates {
				description := candidate.Description
				if candidate.Disabled {
					description = strings.TrimSpace(description + " (not restartable)")
				}
				value := candidate.Value
				if value == "" {
					value = candidate.Label
				}
				items = append(items, completion.Item{
					Label:       candidate.Label,
					Description: description,
					Value:       cmd.SlashCommand + " " + value,
					Disabled:    candidate.Disabled,
					Pinned:      cmd.CompleteArgumentFor != nil,
				})
			}
			return items, true
		}
	}

	return nil, false
}

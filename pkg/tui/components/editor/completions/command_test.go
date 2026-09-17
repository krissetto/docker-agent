package completions

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/tui/commands"
	"github.com/docker/docker-agent/pkg/tui/components/completion"
)

// categoriesWithToolsetRestart mirrors the real "Session" category's
// "Restart Toolset" command, whose label doesn't share a prefix with its
// "/toolset-restart" slash command — the case that exposed the
// inline-completion bug.
func categoriesWithToolsetRestart() []commands.Category {
	return []commands.Category{
		{
			Name: "Session",
			Commands: []commands.Item{
				{ID: "session.exit", Label: "Exit", SlashCommand: "/exit"},
				{ID: "session.toolset.restart", Label: "Restart Toolset", SlashCommand: "/toolset-restart"},
			},
		},
	}
}

func TestCommandCompletionItems(t *testing.T) {
	t.Parallel()

	items := NewCommandCompletion(categoriesWithToolsetRestart()).Items()

	require.Len(t, items, 2)
	labels := map[string]string{}
	for _, item := range items {
		labels[item.Label] = item.Value
	}
	assert.Equal(t, "/toolset-restart", labels["Restart Toolset"])
	assert.Equal(t, "/exit", labels["Exit"])
}

// TestCommandCompletionMatchesToolsetRestartQuery exercises the full pipeline
// the editor uses: items from commandCompletion.Items(), filtered by the
// completion manager in the command completion's own MatchMode. It guards
// against a regression where typing "/toolset-restart" (trigger stripped to
// "toolset-restart") failed to surface the command because filtering only
// looked at Label ("Restart Toolset"), never at Value ("/toolset-restart").
func TestCommandCompletionMatchesToolsetRestartQuery(t *testing.T) {
	t.Parallel()

	cmdCompletion := NewCommandCompletion(categoriesWithToolsetRestart())

	for _, query := range []string{"toolset-restart", "tool", "t"} {
		t.Run(query, func(t *testing.T) {
			t.Parallel()

			m := completion.New()
			m.SetSize(80, 24) // Match the real shell allocation before rendering.
			m.Update(completion.OpenMsg{
				Items:     cmdCompletion.Items(),
				MatchMode: cmdCompletion.MatchMode(),
			})
			_, cmd := m.Update(completion.QueryMsg{Query: query})
			require.NotNil(t, cmd)

			view := m.View()
			assert.Contains(t, view, "Restart Toolset", "toolset-restart command should appear for query %q", query)
		})
	}
}

func TestCommandCompletionMatchModeIsPrefix(t *testing.T) {
	t.Parallel()

	c := NewCommandCompletion(nil)
	assert.Equal(t, completion.MatchPrefix, c.MatchMode())
}

// categoriesWithArgumentCommand adds a command with a CompleteArgument
// provider (mirroring /toolset-restart) alongside one with none, so tests
// can exercise both the hit and the no-provider miss paths.
func categoriesWithArgumentCommand() []commands.Category {
	return []commands.Category{
		{
			Name: "Session",
			Commands: []commands.Item{
				{ID: "session.exit", Label: "Exit", SlashCommand: "/exit"},
				{
					ID:           "session.toolset.restart",
					Label:        "Restart Toolset",
					SlashCommand: "/toolset-restart",
					CompleteArgument: func() []commands.ArgumentCandidate {
						return []commands.ArgumentCandidate{
							{Label: "github", Description: "MCP · ready"},
							{Label: "filesystem", Description: "Built-in · ready", Disabled: true},
						}
					},
				},
			},
		},
	}
}

func TestCommandCompletionArgumentItems_Hit(t *testing.T) {
	t.Parallel()

	c, ok := NewCommandCompletion(categoriesWithArgumentCommand()).(ArgumentCompleter)
	require.True(t, ok, "commandCompletion must implement ArgumentCompleter")

	items, matched := c.ArgumentItems("/toolset-restart gi")
	require.True(t, matched)
	require.Len(t, items, 2)

	assert.Equal(t, "github", items[0].Label)
	assert.Equal(t, "/toolset-restart github", items[0].Value)
	assert.False(t, items[0].Disabled)
	assert.NotContains(t, items[0].Description, "not restartable")

	assert.Equal(t, "filesystem", items[1].Label)
	assert.Equal(t, "/toolset-restart filesystem", items[1].Value)
	assert.True(t, items[1].Disabled)
	assert.Contains(t, items[1].Description, "not restartable")
}

func TestCommandCompletionArgumentItems_UnknownCommand(t *testing.T) {
	t.Parallel()

	c, ok := NewCommandCompletion(categoriesWithArgumentCommand()).(ArgumentCompleter)
	require.True(t, ok)

	items, matched := c.ArgumentItems("/nope something")
	assert.False(t, matched)
	assert.Nil(t, items)
}

func TestCommandCompletionArgumentItems_CommandWithoutProvider(t *testing.T) {
	t.Parallel()

	c, ok := NewCommandCompletion(categoriesWithArgumentCommand()).(ArgumentCompleter)
	require.True(t, ok)

	// /exit exists but has no CompleteArgument, so it must miss just like an
	// unknown command — the popup should not open.
	items, matched := c.ArgumentItems("/exit")
	assert.False(t, matched)
	assert.Nil(t, items)
}

func TestCommandCompletionArgumentItems_NoTrailingArgumentStillMatches(t *testing.T) {
	t.Parallel()

	c, ok := NewCommandCompletion(categoriesWithArgumentCommand()).(ArgumentCompleter)
	require.True(t, ok)

	items, matched := c.ArgumentItems("/toolset-restart")
	require.True(t, matched)
	assert.Len(t, items, 2)
}

// TestCommandCompletionArgumentItems_QueriesFreshOnEachCall is a regression
// test for bug 2 (PR #3728): ArgumentItems must invoke CompleteArgument again
// on every call rather than reusing a result computed by an earlier call, so
// a single Tab always reflects the live toolset set.
func TestCommandCompletionArgumentItems_QueriesFreshOnEachCall(t *testing.T) {
	t.Parallel()

	callCount := 0
	categories := []commands.Category{
		{
			Name: "Session",
			Commands: []commands.Item{
				{
					ID:           "session.toolset.restart",
					Label:        "Restart Toolset",
					SlashCommand: "/toolset-restart",
					CompleteArgument: func() []commands.ArgumentCandidate {
						callCount++
						if callCount == 1 {
							return []commands.ArgumentCandidate{{Label: "oldtool"}}
						}
						return []commands.ArgumentCandidate{{Label: "newtool"}}
					},
				},
			},
		},
	}
	c, ok := NewCommandCompletion(categories).(ArgumentCompleter)
	require.True(t, ok)

	first, matched := c.ArgumentItems("/toolset-restart ")
	require.True(t, matched)
	require.Len(t, first, 1)
	assert.Equal(t, "oldtool", first[0].Label)

	second, matched := c.ArgumentItems("/toolset-restart ")
	require.True(t, matched)
	require.Len(t, second, 1)
	assert.Equal(t, "newtool", second[0].Label, "a second call must reflect the current candidates, not replay the first")
	assert.Equal(t, 2, callCount, "CompleteArgument must be invoked again on each call")
}

func TestContextualArgumentCompletionPreservesRawQueryAndExactValue(t *testing.T) {
	t.Parallel()
	var queries []string
	categories := []commands.Category{{Name: "Pane", Commands: []commands.Item{{
		SlashCommand: "/panes",
		CompleteArgument: func() []commands.ArgumentCandidate {
			t.Fatal("legacy callback must not run when contextual provider exists")
			return nil
		},
		CompleteArgumentFor: func(arg string) []commands.ArgumentCandidate {
			queries = append(queries, arg)
			return []commands.ArgumentCandidate{
				{Label: "Review session", Value: `right "session with spaces"`, Description: "canonical session"},
				{Label: "Unavailable", Value: "right unavailable-id", Disabled: true},
				{Label: "next"},
			}
		},
	}}}}
	provider, ok := NewCommandCompletion(categories).(ArgumentCompleter)
	require.True(t, ok)
	items, matched := provider.ArgumentItems(`/panes right "session with`)
	require.True(t, matched)
	require.Len(t, items, 3)
	assert.Equal(t, []string{`right "session with`}, queries)
	assert.Equal(t, "Review session", items[0].Label)
	assert.True(t, items[0].Pinned, "contextual provider owns filtering and rank")
	assert.Equal(t, `/panes right "session with spaces"`, items[0].Value)
	assert.Equal(t, "/panes right unavailable-id", items[1].Value)
	assert.True(t, items[1].Disabled)
	assert.Equal(t, "/panes next", items[2].Value)
	_, matched = provider.ArgumentItems("/panes  down ")
	require.True(t, matched)
	assert.Equal(t, " down ", queries[1], "contextual callback receives exact whitespace")
}

func TestLegacyArgumentCompletionOptionalExactValue(t *testing.T) {
	t.Parallel()
	provider, ok := NewCommandCompletion([]commands.Category{{Commands: []commands.Item{{
		SlashCommand: "/drop",
		CompleteArgument: func() []commands.ArgumentCandidate {
			return []commands.ArgumentCandidate{
				{Label: "short display", Value: "/fixture/path with spaces.txt"},
				{Label: "legacy-label"},
			}
		},
	}}}}).(ArgumentCompleter)
	require.True(t, ok)
	items, matched := provider.ArgumentItems("/drop ")
	require.True(t, matched)
	require.Len(t, items, 2)
	assert.Equal(t, "/drop /fixture/path with spaces.txt", items[0].Value)
	assert.Equal(t, "/drop legacy-label", items[1].Value)
	assert.False(t, items[0].Pinned, "legacy provider retains ordinary filtering")
	assert.False(t, items[1].Pinned)
}

func TestContextualArgumentResultsBypassDuplicateLabelFiltering(t *testing.T) {
	t.Parallel()
	provider, ok := NewCommandCompletion([]commands.Category{{Commands: []commands.Item{{
		SlashCommand: "/panes",
		CompleteArgumentFor: func(arg string) []commands.ArgumentCandidate {
			if arg != "right review" {
				return nil
			}
			return []commands.ArgumentCandidate{
				{Label: "Review session", Value: "right exact-target", Description: "first relevant result"},
				{Label: "Unavailable session", Value: "right unavailable", Disabled: true},
			}
		},
	}}}}).(ArgumentCompleter)
	require.True(t, ok)
	items, matched := provider.ArgumentItems("/panes right review")
	require.True(t, matched)
	popup := completion.New()
	popup.SetSize(80, 24) // Filtering tests still render inside a real viewport.
	popup.Update(completion.OpenMsg{Items: items, MatchMode: completion.MatchPrefix})
	popup.Update(completion.QueryMsg{Query: "right review"})
	assert.Contains(t, popup.View(), "Review session")
	assert.Contains(t, popup.View(), "Unavailable session")
	assert.True(t, items[1].Disabled)
}

package help_test

import (
	"testing"

	"github.com/docker/docker-agent/pkg/tui/widgets/help"
	"github.com/docker/docker-agent/pkg/tui/widgets/key"
)

type bindings struct{ quit key.Binding }

func (b bindings) ShortHelp() []key.Binding  { return []key.Binding{b.quit} }
func (b bindings) FullHelp() [][]key.Binding { return [][]key.Binding{b.ShortHelp()} }

func TestKeyMap(t *testing.T) {
	t.Parallel()
	var keyMap help.KeyMap = bindings{quit: key.NewBinding(key.WithKeys("q"), key.WithHelp("q", "quit"))}
	if got := keyMap.ShortHelp(); len(got) != 1 || got[0].Help().Desc != "quit" {
		t.Fatalf("unexpected short help: %v", got)
	}
	if got := keyMap.FullHelp(); len(got) != 1 || len(got[0]) != 1 || got[0][0].Help().Key != "q" {
		t.Fatalf("unexpected full help: %v", got)
	}
}

package core

import (
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/require"
)

func TestMapCommandOwnsLeavesAndExplicitTerminalEffects(t *testing.T) {
	t.Parallel()
	type arbitrarySlice []tea.Cmd
	input := arbitrarySlice{CmdHandler("not a sequence")}
	leaves := 0
	leaf := func(msg tea.Msg) tea.Msg { leaves++; return struct{ Inner tea.Msg }{msg} }
	got := MapCommand(CmdHandler(input), leaf)()
	require.IsType(t, struct{ Inner tea.Msg }{}, got, "unrelated slices are application leaves")
	require.Equal(t, 1, leaves)
	for _, tc := range []struct{ owned, framework tea.Cmd }{
		{SetClipboard("text"), tea.SetClipboard("text")},
		{SetPrimaryClipboard("primary"), tea.SetPrimaryClipboard("primary")},
	} {
		require.Equal(t, tc.framework(), MapCommand(tc.owned, leaf)())
	}
	require.Equal(t, 1, leaves, "explicit clipboard is adapted rather than routed")
	require.IsType(t, struct{ Inner tea.Msg }{}, MapCommand(tea.Quit, leaf)(), "private controls have no implicit pane permission")
	require.Nil(t, Sequence(nil))
	require.Nil(t, MapCommand(nil, leaf))
	require.Nil(t, MapCommand(CmdHandler(nil), leaf)())
}

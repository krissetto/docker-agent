package chat

import (
	"testing"

	"github.com/stretchr/testify/require"

	msgtypes "github.com/docker/docker-agent/pkg/tui/messages"
	"github.com/docker/docker-agent/pkg/tui/styles"
)

// Non-split vertical sidebar: hit lookups must subtract the root's blank top
// row and the app padding exactly as handleSidebarClickType does.
func TestSidebarLocalPointMatchesClickMappingInNonSplitPage(t *testing.T) {
	p := newLayoutTestPage(t, msgtypes.SidebarRight)
	p.SetSize(160, 40)
	p.SetMainContentInset(1)
	require.Nil(t, p.splitPresentation, "fixture is the non-split path")
	sl := p.computeSidebarLayout()
	x := styles.AppPadding + sl.sidebarStartX + 3
	y := 5
	localX, localY := p.sidebarLocalPoint(x, y)
	require.Equal(t, x-styles.AppPadding-sl.sidebarStartX, localX)
	require.Equal(t, y-p.mainContentInset, localY, "the root's blank top row is not a sidebar row")
}

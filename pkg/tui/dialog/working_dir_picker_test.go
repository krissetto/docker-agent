package dialog

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"
)

func TestWorkingDirPickerRootHasNoParentDirEntry(t *testing.T) {
	t.Parallel()

	// Get the root of the current working directory
	cwd, err := os.Getwd()
	require.NoError(t, err)
	root := filepath.VolumeName(cwd) + string(filepath.Separator)

	d := NewWorkingDirPickerDialog(t.Context(), nil, nil, nil, root).(*workingDirPickerDialog)

	// Ensure there's no ".." entry in the browse entries
	for _, e := range d.browseEntries {
		if e.name == ".." {
			t.Errorf("root directory should not have a parent dir entry, but got '..'")
		}
	}
}

func TestWorkingDirPickerEmptyInitialDirUsesGetwd(t *testing.T) {
	t.Parallel()

	// Pass an empty string for the initial directory.
	// NewWorkingDirPickerDialog should fall back to os.Getwd().
	d := NewWorkingDirPickerDialog(t.Context(), nil, nil, nil, "").(*workingDirPickerDialog)

	cwd, err := os.Getwd()
	require.NoError(t, err)

	require.Equal(t, cwd, d.currentDir, "empty initial directory should fall back to current working directory")
}

func TestWorkingDirectorySelectedSectionUsesColorOnly(t *testing.T) {
	d := &workingDirPickerDialog{}
	for _, section := range dirPickerSectionOrder {
		d.section = section
		view := d.renderTabs(100)
		assertToolActionsNotUnderlined(t, view)
		require.Contains(t, view, "Browse")
		require.Contains(t, view, "Recent")
		require.Contains(t, view, "Pinned")
		require.Equal(t, section, d.section, "render preserves the selected section")
	}
}

func TestWorkingDirectorySharedHeaderSpacingAndTabHitGeometry(t *testing.T) {
	root := t.TempDir()
	d := NewWorkingDirPickerDialog(t.Context(), []string{root + "/recent"}, []string{root + "/pinned"}, nil, root).(*workingDirPickerDialog)
	for _, size := range [][2]int{{120, 40}, {40, 12}, {24, 6}, {8, 3}, {1, 1}, {120, 40}} {
		for _, section := range dirPickerSectionOrder {
			d.setSection(section)
			d.SetSize(size[0], size[1])
			view := d.View()
			row, col := d.Position()
			require.LessOrEqual(t, lipgloss.Width(view), size[0])
			require.LessOrEqual(t, lipgloss.Height(view), size[1])
			tabY, visible := d.headerRow(2)
			if !visible {
				for y := row; y < row+lipgloss.Height(view); y++ {
					require.Equal(t, -1, d.tabClickTarget(d.bodyX, y))
				}
				continue
			}
			lines := strings.Split(ansi.Strip(view), "\n")
			require.Contains(t, lines[tabY-row], "Browse")
			if size[1] >= 12 {
				titleY, titleVisible := d.headerRow(0)
				require.True(t, titleVisible)
				require.Equal(t, titleY+3, tabY, "title gap and divider precede tabs")
				require.Empty(t, strings.Trim(strings.TrimSpace(lines[titleY-row+1]), "│ "))
			}
			for _, region := range d.tabRegions {
				x := d.bodyX + region.xStart
				if x >= d.bodyX+d.bodyWidth-d.bodyScroll.ReservedCols() {
					continue
				}
				require.Equal(t, int(region.section), d.tabClickTarget(x, tabY))
				require.Equal(t, -1, d.tabClickTarget(x, tabY-1))
				require.Equal(t, -1, d.tabClickTarget(x, tabY+1))
			}
			require.Equal(t, -1, d.tabClickTarget(col, tabY), "border is not a tab")
			require.Equal(t, -1, d.tabClickTarget(col+lipgloss.Width(view), tabY))
		}
	}
}

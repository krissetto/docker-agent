package image

import (
	"strconv"
	"strings"

	"github.com/charmbracelet/x/ansi"
)

// MarkerColumns identifies internal image markers and their horizontal extent.
func MarkerColumns(sequence string) (columns int, marker bool) {
	if !strings.HasPrefix(sequence, markerPrefix) {
		return 0, false
	}
	fields := strings.Split(strings.TrimSuffix(strings.TrimPrefix(sequence, markerPrefix), "\x1b\\"), ";")
	if len(fields) != 4 && (len(fields) != 5 || fields[4] != "preview") {
		return 0, true
	}
	columns, err := strconv.Atoi(fields[1])
	if err != nil || columns < 1 {
		return 0, true
	}
	return columns, true
}

// StripMarkers removes graphics from transient text-only animation frames.
func StripMarkers(content string) string {
	parser := ansi.GetParser()
	defer ansi.PutParser(parser)
	var out strings.Builder
	var state byte
	for content != "" {
		sequence, _, consumed, next := ansi.DecodeSequence(content, state, parser)
		if consumed == 0 {
			break
		}
		if _, marker := MarkerColumns(sequence); !marker {
			out.WriteString(sequence)
		}
		state, content = next, content[consumed:]
	}
	return out.String()
}

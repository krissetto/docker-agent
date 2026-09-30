package root

import (
	"bytes"
	"errors"
	"io"
	"testing"

	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResetInheritedTerminalStyleBeforeFirstErase(t *testing.T) {
	var output bytes.Buffer
	output.WriteString("\x1b[48;2;17;34;51m")
	require.NoError(t, resetInheritedTerminalStyle(&output, true))
	output.WriteString(ansi.EraseEntireScreen)
	parser := ansi.GetParser()
	defer ansi.PutParser(parser)
	var state byte
	pen := uv.Style{}
	content := output.String()
	erased := false
	for content != "" {
		seq, _, n, next := ansi.DecodeSequence(content, state, parser)
		require.Positive(t, n)
		if ansi.HasCsiPrefix(seq) && parser.Command() == 'm' {
			uv.ReadStyle(parser.Params(), &pen)
		}
		if seq == ansi.EraseEntireScreen {
			assert.Nil(t, pen.Bg, "first erase must use terminal-default background, not inherited explicit RGB")
			erased = true
		}
		state, content = next, content[n:]
	}
	require.True(t, erased)
	output.Reset()
	require.NoError(t, resetInheritedTerminalStyle(&output, false))
	assert.Empty(t, output.String(), "non-terminal output receives no initialization escapes")
}

type terminalResetFailure struct{ short bool }

func (w terminalResetFailure) Write(p []byte) (int, error) {
	if w.short {
		return 0, nil
	}
	return 0, errors.New("output failed")
}
func TestTerminalStyleResetReportsWriteErrors(t *testing.T) {
	assert.ErrorContains(t, resetInheritedTerminalStyle(terminalResetFailure{}, true), "output failed")
	assert.ErrorIs(t, resetInheritedTerminalStyle(terminalResetFailure{short: true}, true), io.ErrShortWrite)
}

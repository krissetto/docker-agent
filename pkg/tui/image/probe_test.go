//go:build !windows

package image

import (
	"bytes"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/creack/pty"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSupportsKittyGraphics(t *testing.T) {
	t.Parallel()

	t.Run("non-terminal", func(t *testing.T) {
		file, err := os.Open(os.DevNull)
		require.NoError(t, err)
		defer file.Close()

		start := time.Now()
		assert.False(t, SupportsKittyGraphics(file, file))
		assert.Less(t, time.Since(start), kittyProbeTimeout)
	})

	t.Run("supported", func(t *testing.T) {
		ptmx, tty, err := pty.Open()
		require.NoError(t, err)
		defer ptmx.Close()
		defer tty.Close()

		go answerKittyProbe(ptmx, kittyProbeOK)
		assert.True(t, SupportsKittyGraphics(tty, tty))
	})

	t.Run("unsupported", func(t *testing.T) {
		ptmx, tty, err := pty.Open()
		require.NoError(t, err)
		defer ptmx.Close()
		defer tty.Close()

		go answerKittyProbe(ptmx, "\x1b_Gi="+kittyProbeID+";ENOTSUP\x1b\\")
		assert.False(t, SupportsKittyGraphics(tty, tty))
	})
}

func answerKittyProbe(terminal *os.File, response string) {
	var seen []byte
	buf := make([]byte, 128)
	for {
		n, err := terminal.Read(buf)
		if err != nil {
			return
		}
		seen = append(seen, buf[:n]...)
		if bytes.Contains(seen, []byte(kittyProbeQuery)) {
			_, _ = terminal.WriteString(response)
			return
		}
	}
}

func TestGhosttyStyleProbeSupportsExplicitPreviewWithAutomaticImagesOff(t *testing.T) {
	t.Setenv("TERM", "xterm-ghostty")
	t.Setenv("TERM_PROGRAM", "ghostty")
	ptmx, tty, err := pty.Open()
	require.NoError(t, err)
	defer ptmx.Close()
	defer tty.Close()
	go func() {
		var seen []byte
		buf := make([]byte, 128)
		for {
			n, err := ptmx.Read(buf)
			if err != nil {
				return
			}
			seen = append(seen, buf[:n]...)
			if bytes.Contains(seen, []byte(kittyProbeQuery)) {
				_, _ = ptmx.WriteString("\x1b[?1;2c" + kittyProbeOK[:8])
				time.Sleep(time.Millisecond)
				_, _ = ptmx.WriteString(kittyProbeOK[8:])
				return
			}
		}
	}()
	supported := SupportsKittyGraphics(tty, tty)
	require.True(t, supported, "active response, not TERM whitelist, establishes capability")
	var out bytes.Buffer
	writer := NewWriter(&out)
	writer.SetSupported(supported)
	writer.SetEnabled(false)
	writer.SetContent(strings.Join(RenderPreviewMarkers(Inline{PNGData: []byte("image"), Width: 10, Height: 10}, 20), "\n"))
	_, err = writer.Write([]byte("frame"))
	require.NoError(t, err)
	assert.Contains(t, out.String(), "a=p,i=")
	assert.False(t, writer.RenderingEnabled())
}

func TestKittyProbeRequiresMatchingPositiveResponse(t *testing.T) {
	for _, response := range []string{"\x1b_Gi=9999;OK\x1b\\", "", "\x1b_Gi=" + kittyProbeID + ";ENOENT\x1b\\"} {
		ptmx, tty, err := pty.Open()
		require.NoError(t, err)
		go answerKittyProbe(ptmx, response)
		assert.False(t, SupportsKittyGraphics(tty, tty))
		assert.NoError(t, ptmx.Close())
		assert.NoError(t, tty.Close())
	}
}

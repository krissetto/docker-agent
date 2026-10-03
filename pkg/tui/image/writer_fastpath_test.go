package image

import (
	"bytes"
	"encoding/base64"
	"io"
	"strconv"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"
)

// Keep the prior parser as a deterministic byte/overlay oracle and benchmark.
func priorExtractOverlays(content string) (string, []overlay) {
	lines := strings.Split(content, "\n")
	overlays := extractMarkerOverlays(lines)
	for y, line := range lines {
		for {
			start := strings.Index(line, "\x1b_G")
			if start < 0 {
				break
			}
			x := ansi.StringWidth(line[:start])
			var encoded strings.Builder
			cols, rows := 0, 0
			end := start
			for strings.HasPrefix(line[end:], "\x1b_G") {
				stop := strings.Index(line[end+3:], "\x1b\\")
				if stop < 0 {
					end += 3
					break
				}
				stop += end + 3
				body := line[end+3 : stop]
				params, payload, _ := strings.Cut(body, ";")
				for param := range strings.SplitSeq(params, ",") {
					key, value, ok := strings.Cut(param, "=")
					if !ok {
						continue
					}
					switch key {
					case "c":
						cols, _ = strconv.Atoi(value)
					case "r":
						rows, _ = strconv.Atoi(value)
					}
				}
				encoded.WriteString(payload)
				end = stop + 2
			}
			data, err := base64.StdEncoding.DecodeString(encoded.String())
			if err == nil && len(data) > 0 && cols > 0 && rows > 0 {
				id := imageID(data)
				overlays = append(overlays, overlay{id: id, png: data, x: x, y: y, cols: cols, rows: rows})
			}
			line = line[:start] + line[end:]
		}
		lines[y] = line
	}
	return strings.Join(lines, "\n"), overlays
}

func TestExtractOverlaysMarkerFreePreservesBytesWithoutAllocations(t *testing.T) {
	for _, content := range []string{
		"", "plain", "header\n\ntrailing  \n", "λ界 👩‍💻 é",
		"\x1b[31mred\x1b[m\n\x1b[48;2;10;20;30m padding  \x1b[m",
		"\x1b]8;;https://example.test\x1b\\link\x1b]8;;\x1b\\",
		"\x1b_other-apc\x1b\\", "\x1b_cagent-imag", "\x1b_",
	} {
		clean, overlays := extractOverlays(content)
		require.Equal(t, content, clean)
		require.Nil(t, overlays)
		want, wantOverlays := priorExtractOverlays(content)
		require.Equal(t, want, clean)
		require.Equal(t, wantOverlays, overlays)
		require.Zero(t, testing.AllocsPerRun(100, func() { extractOverlays(content) }))
	}
}

func TestExtractOverlaysFastpathKeepsMarkerParsing(t *testing.T) {
	img := Inline{PNGData: []byte("png-data"), Width: 100, Height: 100}
	markers := strings.Join(RenderMarkers(img, 24), "\n")
	for _, content := range []string{
		markers,
		"header\n" + KittySequence([]byte("png-data"), 4, 2) + "tail\n",
		markers + "\n" + KittySequence([]byte("other-png"), 3, 2),
		markerPrefix, markerPrefix + "bad\x1b\\tail", "\x1b_G", "\x1b_Ginvalid\x1b\\tail",
	} {
		want, wantOverlays := priorExtractOverlays(content)
		got, overlays := extractOverlays(content)
		require.Equal(t, want, got)
		require.Equal(t, wantOverlays, overlays)
	}
}

func TestWriterMarkerFreeFrameClearsOverlaysAndFlushes(t *testing.T) {
	for _, policy := range []string{"enabled", "disabled", "unsupported"} {
		t.Run(policy, func(t *testing.T) {
			var output bytes.Buffer
			writer := NewWriter(&output)
			writer.SetContent(KittySequence([]byte("png-data"), 4, 2))
			_, err := writer.Write([]byte("image frame"))
			require.NoError(t, err)
			require.True(t, writer.active)
			output.Reset()
			switch policy {
			case "disabled":
				writer.SetEnabled(false)
			case "unsupported":
				writer.SetSupported(false)
			}
			content := "\x1b[31mtext only\x1b[m\n"
			require.Equal(t, content, writer.SetContent(content))
			require.Empty(t, writer.overlays)
			require.True(t, writer.RequestFlush(), "identical text frames still clear prior graphics")
			_, err = writer.Write(nil)
			require.NoError(t, err)
			require.Contains(t, output.String(), "a=d,d=a")
			require.False(t, writer.active)
			require.False(t, writer.RequestFlush())
			require.Zero(t, testing.AllocsPerRun(100, func() { writer.SetContent(content) }))
		})
	}
}

func BenchmarkExtractOverlaysMarkerFree(b *testing.B) {
	content := strings.Repeat("\x1b[31mstatic λ界\x1b[m  \x1b]8;;https://example.test\x1b\\link\x1b]8;;\x1b\\"+strings.Repeat(" ", 100)+"\n", 48)
	for _, impl := range []struct {
		name    string
		extract func(string) (string, []overlay)
	}{{"prior", priorExtractOverlays}, {"fast", extractOverlays}} {
		b.Run(impl.name, func(b *testing.B) {
			b.ReportAllocs()
			b.SetBytes(int64(len(content)))
			for b.Loop() {
				impl.extract(content)
			}
		})
	}
	b.Run("SetContent", func(b *testing.B) {
		writer := NewWriter(io.Discard)
		b.ReportAllocs()
		b.SetBytes(int64(len(content)))
		b.ResetTimer()
		for b.Loop() {
			writer.SetContent(content)
		}
	})
}

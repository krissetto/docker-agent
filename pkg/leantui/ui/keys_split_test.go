package ui

import (
	"math/rand/v2"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// feedChunks decodes each chunk in turn with one parser, as a terminal read loop does.
func feedChunks(chunks ...string) []Key {
	p := &InputParser{}
	var out []Key
	for _, chunk := range chunks {
		out = append(out, p.Feed([]byte(chunk))...)
	}
	return out
}

var splitStreams = []struct {
	name   string
	stream string
	want   []Key
}{
	{"up", "\x1b[A", []Key{{Typ: KeyUp}}},
	{"down", "\x1b[B", []Key{{Typ: KeyDown}}},
	{"right", "\x1b[C", []Key{{Typ: KeyRight}}},
	{"left", "\x1b[D", []Key{{Typ: KeyLeft}}},
	{"ctrl-right", "\x1b[1;5C", []Key{{Typ: KeyWordRight}}},
	{"alt-up", "\x1b[1;3A", []Key{{Typ: KeyAltUp}}},
	{"delete", "\x1b[3~", []Key{{Typ: KeyDelete}}},
	{"shift-tab", "\x1b[Z", []Key{{Typ: KeyShiftTab}}},
	{"alt-enter csi-u", "\x1b[13;3u", []Key{{Typ: KeyAltEnter}}},
	{"shift-enter csi-u", "\x1b[13;2u", []Key{{Typ: KeyShiftEnter}}},
	{"paste", "\x1b[200~hello\x1b[201~", []Key{{Typ: KeyPaste, Runes: []rune("hello")}}},
	{"paste containing escape", "\x1b[200~a\x1b[Ab\x1b[201~", []Key{{Typ: KeyPaste, Runes: []rune("a\x1b[Ab")}}},
	{"sgr mouse press", "\x1b[<0;10;5M", nil},
	{"sgr mouse release", "\x1b[<0;10;5m", nil},
	{"osc color reply", "\x1b]11;rgb:0000/0000/0000\x1b\\", []Key{{Typ: KeyBackgroundDark}}},
	{"mixed", "x\x1b[A\x1b[1;5Cy\x1b[<0;1;1Mz\r", []Key{
		{Typ: KeyRune, Runes: []rune{'x'}},
		{Typ: KeyUp},
		{Typ: KeyWordRight},
		{Typ: KeyRune, Runes: []rune{'y'}},
		{Typ: KeyRune, Runes: []rune{'z'}},
		{Typ: KeyEnter},
	}},
}

func TestSplitStreamsDecodeAtEveryBoundary(t *testing.T) {
	t.Parallel()
	for _, tt := range splitStreams {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tt.want, feedChunks(tt.stream), "atomic decode")
			for cut := 1; cut < len(tt.stream); cut++ {
				got := feedChunks(tt.stream[:cut], tt.stream[cut:])
				assert.Equal(t, tt.want, got, "split at byte %d", cut)
			}
		})
	}
}

func TestSplitStreamsDecodeWhenDeliveredInRandomChunks(t *testing.T) {
	t.Parallel()
	rng := rand.New(rand.NewPCG(1, 2))
	for _, tt := range splitStreams {
		for range 200 {
			var chunks []string
			start := 0
			for start < len(tt.stream) {
				end := start + 1 + rng.IntN(len(tt.stream)-start)
				chunks = append(chunks, tt.stream[start:end])
				start = end
			}
			assert.Equal(t, tt.want, feedChunks(chunks...), "%s in chunks %q", tt.name, chunks)
		}
	}
}

func TestLoneEscapeIsHeldUntilExpired(t *testing.T) {
	t.Parallel()
	p := &InputParser{}
	assert.Empty(t, p.Feed([]byte("\x1b")))
	assert.True(t, p.Waiting())
	assert.Equal(t, []Key{{Typ: KeyEsc}}, p.Expire())
	assert.False(t, p.Waiting())
	assert.Equal(t, []Key{{Typ: KeyRune, Runes: []rune{'a'}}}, p.Feed([]byte("a")))
}

func TestAltEnterAsOneRead(t *testing.T) {
	t.Parallel()
	assert.Equal(t, []Key{{Typ: KeyAltEnter}}, feedChunks("\x1b\r"))
}

func TestMouseReportsStaySwallowedWhenSplit(t *testing.T) {
	t.Parallel()
	for _, report := range []string{"\x1b[<0;10;5M", "\x1b[<64;120;40M", "\x1b[<0;10;5m"} {
		for cut := 1; cut < len(report); cut++ {
			assert.Empty(t, feedChunks(report[:cut], report[cut:]), "report %q split at %d", report, cut)
		}
	}
}

func TestEnterAfterBareEscBracketIsNotLost(t *testing.T) {
	t.Parallel()
	assert.Equal(t, []Key{{Typ: KeyEnter}}, feedChunks("\x1b[", "\r"))
}

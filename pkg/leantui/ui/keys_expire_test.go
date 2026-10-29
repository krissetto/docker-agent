package ui

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestBareEscBracketExpiresWithoutSwallowingNextKey(t *testing.T) {
	t.Parallel()
	p := &InputParser{}
	assert.Empty(t, p.Feed([]byte("\x1b[")))
	assert.True(t, p.Waiting())
	assert.Empty(t, p.Expire(), "Alt+[ alone is dropped, as upstream does")
	assert.Equal(t, []Key{{Typ: KeyRune, Runes: []rune{'x'}}}, p.Feed([]byte("x")))
}

// Within the expiry window an unknown CSI final byte is consumed, as upstream does.
func TestBareEscBracketThenLetterWithinWindowIsCSIFinal(t *testing.T) {
	t.Parallel()
	p := &InputParser{}
	assert.Empty(t, p.Feed([]byte("\x1b[")))
	assert.Empty(t, p.Feed([]byte("x")))
	assert.False(t, p.Waiting())
}

func TestSS3WaitsForItsFinalByte(t *testing.T) {
	t.Parallel()
	p := &InputParser{}
	assert.Empty(t, p.Feed([]byte("\x1bO")))
	assert.True(t, p.Waiting())
	assert.Equal(t, []Key{{Typ: KeyUp}}, p.Feed([]byte("A")))

	p = &InputParser{}
	assert.Empty(t, p.Feed([]byte("\x1bO")))
	assert.Empty(t, p.Expire())
}

func TestOSCAndPasteAreNotWaiting(t *testing.T) {
	t.Parallel()
	p := &InputParser{}
	assert.Empty(t, p.Feed([]byte("\x1b]11;rgb:0000/0000/0000")))
	assert.False(t, p.Waiting())
	assert.Empty(t, p.Expire())
	assert.Equal(t, []Key{{Typ: KeyBackgroundDark}}, p.Feed([]byte("\x1b\\")))

	p = &InputParser{}
	assert.Empty(t, p.Feed([]byte("\x1b[200~hi")))
	assert.False(t, p.Waiting())
	assert.Empty(t, p.Expire())
}

func TestControlByteAbortsCSIPrefix(t *testing.T) {
	t.Parallel()
	assert.Equal(t, []Key{{Typ: KeyEnter}}, feedChunks("\x1b[", "\r"))
	assert.Equal(t, []Key{{Typ: KeyEnter}}, feedChunks("\x1b[1", "\r"))
	assert.Equal(t, []Key{{Typ: KeyUp}}, feedChunks("\x1b[1", "\x1b[A"))
}

func TestNonASCIIByteAbortsCSIPrefix(t *testing.T) {
	t.Parallel()
	assert.Equal(t, []Key{{Typ: KeyRune, Runes: []rune{'é'}}}, feedChunks("\x1b[é"))
}

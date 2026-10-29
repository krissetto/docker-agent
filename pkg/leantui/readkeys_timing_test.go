package leantui

import (
	"io"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/leantui/ui"
)

const expireTestTimeout = 20 * time.Millisecond

func awaitKey(t *testing.T, keys <-chan ui.Key) ui.Key {
	t.Helper()
	select {
	case k := <-keys:
		return k
	case <-time.After(readKeysWait):
		t.Fatal("no key delivered")
		return ui.Key{}
	}
}

func TestReadKeysFlushesLoneEscapeAfterTimeout(t *testing.T) {
	t.Parallel()

	pr, pw := io.Pipe()
	keys := make(chan ui.Key, 8)
	done := make(chan struct{})
	returned := make(chan struct{})
	go func() {
		defer close(returned)
		readKeysWithin(pr, keys, done, expireTestTimeout)
	}()
	_, err := pw.Write([]byte("\x1b"))
	require.NoError(t, err)
	assert.Equal(t, ui.Key{Typ: ui.KeyEsc}, awaitKey(t, keys))
	_, err = pw.Write([]byte("b"))
	require.NoError(t, err)
	assert.Equal(t, ui.Key{Typ: ui.KeyRune, Runes: []rune{'b'}}, awaitKey(t, keys))
	require.NoError(t, pw.Close())
	awaitReturn(t, returned, "readKeys did not return at EOF")
}

func TestReadKeysDropsExpiredPrefixThenDecodesNextKey(t *testing.T) {
	t.Parallel()

	pr, pw := io.Pipe()
	keys := make(chan ui.Key, 8)
	returned := make(chan struct{})
	go func() {
		defer close(returned)
		readKeysWithin(pr, keys, make(chan struct{}), expireTestTimeout)
	}()
	_, err := pw.Write([]byte("\x1b["))
	require.NoError(t, err)
	<-time.After(200 * time.Millisecond)
	_, err = pw.Write([]byte("x"))
	require.NoError(t, err)
	assert.Equal(t, ui.Key{Typ: ui.KeyRune, Runes: []rune{'x'}}, awaitKey(t, keys))
	require.NoError(t, pw.Close())
	awaitReturn(t, returned, "readKeys did not return at EOF")
}

func TestReadKeysShutdownWhileWaiting(t *testing.T) {
	t.Parallel()

	pr, pw := io.Pipe()
	t.Cleanup(func() { _ = pr.Close(); _ = pw.Close() })
	keys := make(chan ui.Key, 8)
	done := make(chan struct{})
	returned := make(chan struct{})
	go func() {
		defer close(returned)
		readKeysWithin(pr, keys, done, time.Hour)
	}()
	_, err := pw.Write([]byte("\x1b"))
	require.NoError(t, err)
	close(done)
	awaitReturn(t, returned, "readKeys hung while a sequence was waiting")
	assert.Empty(t, drainKeys(keys), "no expiry may fire after shutdown")
}

func TestReadKeysReaderErrorWhileWaitingFlushesEscape(t *testing.T) {
	t.Parallel()

	reader := &stepReader{steps: []readStep{{data: "\x1b", err: io.EOF}}}
	keys := make(chan ui.Key, 8)
	awaitReturn(t, startReadKeysWithin(reader, keys, make(chan struct{}), time.Hour), "readKeys did not return after EOF")
	assert.Equal(t, []ui.Key{{Typ: ui.KeyEsc}}, drainKeys(keys))
}

func startReadKeysWithin(r io.Reader, keys chan<- ui.Key, done <-chan struct{}, timeout time.Duration) <-chan struct{} {
	returned := make(chan struct{})
	go func() {
		defer close(returned)
		readKeysWithin(r, keys, done, timeout)
	}()
	return returned
}

package leantui

import (
	"io"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/leantui/ui"
)

// A bare ESC [ must not swallow a key typed after it has had time to expire.
func TestReadKeysBareEscBracketDoesNotSwallowLaterKey(t *testing.T) {
	t.Parallel()

	pr, pw := io.Pipe()
	keys := make(chan ui.Key, 8)
	returned := startReadKeys(pr, keys, make(chan struct{}))
	_, err := pw.Write([]byte("\x1b["))
	require.NoError(t, err)
	<-time.After(200 * time.Millisecond)
	_, err = pw.Write([]byte("x"))
	require.NoError(t, err)
	require.NoError(t, pw.Close())
	awaitReturn(t, returned, "readKeys did not return at EOF")
	assert.Equal(t, []ui.Key{{Typ: ui.KeyRune, Runes: []rune{'x'}}}, drainKeys(keys))
}

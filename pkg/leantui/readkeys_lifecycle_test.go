package leantui

import (
	"errors"
	"io"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/muesli/cancelreader"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/leantui/ui"
)

const readKeysWait = 5 * time.Second

type readStep struct {
	data string
	err  error
}

// stepReader serves scripted reads, then io.EOF.
type stepReader struct {
	mu    sync.Mutex
	steps []readStep
	reads int
}

func (r *stepReader) Read(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reads++
	if len(r.steps) == 0 {
		return 0, io.EOF
	}
	step := r.steps[0]
	r.steps = r.steps[1:]
	return copy(p, step.data), step.err
}

func (r *stepReader) readCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.reads
}

// uncancelableReader blocks in Read until released and refuses to cancel.
type uncancelableReader struct {
	release chan struct{}
	cancels int
	mu      sync.Mutex
}

func (r *uncancelableReader) Read([]byte) (int, error) {
	<-r.release
	return 0, io.EOF
}

func (r *uncancelableReader) Cancel() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cancels++
	return false
}

func (*uncancelableReader) Close() error { return nil }

// blockingCancelReader yields one chunk, then blocks until Cancel releases it.
type blockingCancelReader struct {
	data     string
	served   bool
	blocked  chan struct{}
	release  chan struct{}
	finished chan struct{}
	once     sync.Once
	blockOne sync.Once
}

func (r *blockingCancelReader) Read(p []byte) (int, error) {
	if !r.served {
		r.served = true
		return copy(p, r.data), nil
	}
	r.blockOne.Do(func() { close(r.blocked) })
	<-r.release
	close(r.finished)
	return 0, cancelreader.ErrCanceled
}

func (r *blockingCancelReader) Cancel() bool {
	r.once.Do(func() { close(r.release) })
	return true
}

func (*blockingCancelReader) Close() error { return nil }

// finishReader marks when the wrapped Read call has returned.
type finishReader struct {
	cancelreader.CancelReader

	entered  chan struct{}
	finished chan struct{}
	enterOne sync.Once
	finOne   sync.Once
}

func (r *finishReader) Read(p []byte) (int, error) {
	r.enterOne.Do(func() { close(r.entered) })
	defer r.finOne.Do(func() { close(r.finished) })
	return r.CancelReader.Read(p)
}

func newPipeCancelReader(t *testing.T) (cancelreader.CancelReader, *os.File) {
	t.Helper()
	r, w, err := os.Pipe()
	require.NoError(t, err)
	cr, err := cancelreader.NewReader(r)
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = cr.Close()
		_ = r.Close()
		_ = w.Close()
	})
	return cr, w
}

func startReadKeys(r io.Reader, keys chan<- ui.Key, done <-chan struct{}) <-chan struct{} {
	returned := make(chan struct{})
	go func() {
		defer close(returned)
		readKeys(r, keys, done)
	}()
	return returned
}

func awaitReturn(t *testing.T, returned <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-returned:
	case <-time.After(readKeysWait):
		t.Fatalf("%s", what)
	}
}

func drainKeys(keys <-chan ui.Key) []ui.Key {
	var out []ui.Key
	for {
		select {
		case k := <-keys:
			out = append(out, k)
		default:
			return out
		}
	}
}

func TestReadKeysReturnsAfterReadError(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		step readStep
		want []ui.Key
	}{
		{
			name: "data returned with error is fed before return",
			step: readStep{data: "ab", err: io.ErrUnexpectedEOF},
			want: []ui.Key{{Typ: ui.KeyRune, Runes: []rune{'a'}}, {Typ: ui.KeyRune, Runes: []rune{'b'}}},
		},
		{
			name: "error without data returns without keys",
			step: readStep{err: errors.New("read failed")},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			reader := &stepReader{steps: []readStep{tt.step}}
			keys := make(chan ui.Key, 8)
			awaitReturn(t, startReadKeys(reader, keys, make(chan struct{})), "readKeys did not return after a read error")
			assert.Equal(t, tt.want, drainKeys(keys))
			assert.Equal(t, 1, reader.readCount(), "no read may follow an error")
		})
	}
}

func TestReadKeysJoinsCancelledTerminalReader(t *testing.T) {
	t.Parallel()

	cr, _ := newPipeCancelReader(t)
	reader := &finishReader{CancelReader: cr, entered: make(chan struct{}), finished: make(chan struct{})}
	done, keys := make(chan struct{}), make(chan ui.Key, 1)
	returned := startReadKeys(reader, keys, done)
	select {
	case <-reader.entered:
	case <-time.After(readKeysWait):
		t.Fatal("readKeys never reached Read")
	}
	close(done)
	awaitReturn(t, returned, "readKeys did not cancel and join the blocked terminal reader")
	select {
	case <-reader.finished:
	default:
		t.Fatal("readKeys returned while the canceled Read was still running")
	}
}

func TestReadKeysReturnsWhenReaderCannotBeCancelled(t *testing.T) {
	t.Parallel()

	t.Run("cancel reports failure", func(t *testing.T) {
		t.Parallel()
		reader := &uncancelableReader{release: make(chan struct{})}
		t.Cleanup(func() { close(reader.release) })
		done, keys := make(chan struct{}), make(chan ui.Key, 1)
		returned := startReadKeys(reader, keys, done)
		close(done)
		awaitReturn(t, returned, "readKeys hung on a reader that cannot be cancelled")
		reader.mu.Lock()
		defer reader.mu.Unlock()
		assert.Equal(t, 1, reader.cancels)
	})

	t.Run("reader is not a CancelReader", func(t *testing.T) {
		t.Parallel()
		pr, pw := io.Pipe()
		t.Cleanup(func() { _ = pr.Close(); _ = pw.Close() })
		done, keys := make(chan struct{}), make(chan ui.Key, 1)
		returned := startReadKeys(pr, keys, done)
		close(done)
		awaitReturn(t, returned, "readKeys hung on a blocked non-cancelable reader")
	})
}

func TestReadKeysJoinsReaderWhenStoppedWhileSending(t *testing.T) {
	t.Parallel()

	reader := &blockingCancelReader{data: "x", blocked: make(chan struct{}), release: make(chan struct{}), finished: make(chan struct{})}
	done, keys := make(chan struct{}), make(chan ui.Key)
	returned := startReadKeys(reader, keys, done)
	select {
	case <-reader.blocked:
	case <-time.After(readKeysWait):
		t.Fatal("reader never blocked after delivering its chunk")
	}
	close(done)
	awaitReturn(t, returned, "readKeys remained blocked sending a key after cancellation")
	select {
	case <-reader.finished:
	default:
		t.Fatal("readKeys returned before the blocked reader was joined")
	}
	assert.Empty(t, drainKeys(keys))
}

// Mirrors Run's runExternal: stop and cancel the current reader, join it, then
// start a fresh reader on the same channels.
func TestReadKeysRestartsOnFreshReaderAfterStop(t *testing.T) {
	t.Parallel()

	keys := make(chan ui.Key, 4)
	first, _ := newPipeCancelReader(t)
	stop1 := make(chan struct{})
	returned1 := startReadKeys(first, keys, stop1)
	close(stop1)
	first.Cancel()
	awaitReturn(t, returned1, "first key reader did not join on stop")

	second, w := newPipeCancelReader(t)
	stop2 := make(chan struct{})
	returned2 := startReadKeys(second, keys, stop2)
	_, err := w.WriteString("k")
	require.NoError(t, err)
	select {
	case key := <-keys:
		assert.Equal(t, ui.Key{Typ: ui.KeyRune, Runes: []rune{'k'}}, key)
	case <-time.After(readKeysWait):
		t.Fatal("restarted key reader delivered no key")
	}
	close(stop2)
	awaitReturn(t, returned2, "restarted key reader did not join on stop")
}

// Input bytes, including mouse reports, must reach the parser exactly as read.
func TestReadKeysMatchesInputParserPerRead(t *testing.T) {
	t.Parallel()

	chunks := []string{
		"a",
		"\x1b[<0;10;5M",
		"\x1b[<0;10;5m",
		"\x1b[200~pa",
		"ste\x1b[201~",
		"\x1b]11;rgb:0000/0000/0000\x1b\\",
		"\x1b[A",
		"\x1b[1;5C",
		"é",
		"\r",
	}
	parser := &ui.InputParser{}
	var want []ui.Key
	for _, chunk := range chunks {
		want = append(want, parser.Feed([]byte(chunk))...)
	}

	pr, pw := io.Pipe()
	go func() {
		for _, chunk := range chunks {
			if _, err := pw.Write([]byte(chunk)); err != nil {
				return
			}
		}
		_ = pw.Close()
	}()
	keys := make(chan ui.Key, 128)
	awaitReturn(t, startReadKeys(pr, keys, make(chan struct{})), "readKeys did not return at EOF")
	assert.Equal(t, want, drainKeys(keys))
}

func TestInputParserSwallowsWholeMouseReports(t *testing.T) {
	t.Parallel()

	parser := &ui.InputParser{}
	assert.Empty(t, parser.Feed([]byte("\x1b[<0;10;5M")))
	assert.Empty(t, parser.Feed([]byte("\x1b[<0;10;5m")))
}

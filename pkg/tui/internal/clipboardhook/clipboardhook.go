// Package clipboardhook routes platform clipboard writes through one swappable
// writer so tests can observe them without touching the host clipboard.
package clipboardhook

import (
	"sync/atomic"

	"github.com/atotto/clipboard"
)

type writer struct{ fn func(string) error }

var current = func() *atomic.Pointer[writer] {
	var p atomic.Pointer[writer]
	p.Store(&writer{fn: clipboard.WriteAll})
	return &p
}()

// WriteAll writes text to the platform clipboard through the active writer.
func WriteAll(text string) error { return current.Load().fn(text) }

// Swap installs fn as the active writer and returns a function restoring the previous one.
func Swap(fn func(string) error) (restore func()) {
	previous := current.Swap(&writer{fn: fn})
	return func() { current.Store(previous) }
}

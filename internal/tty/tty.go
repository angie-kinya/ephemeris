// Package tty manages the local terminal: raw mode and window-size changes
package tty

import (
	"context"
	"os"
	"os/signal"
	"sync"
	"syscall"

	"golang.org/x/term"

	"github.com/angie-kinya/ephemeris/internal/backend"
)

func IsTerminal(f *os.File) bool { return term.IsTerminal(int(f.Fd())) }

// Raw switches the terminal to raw mode: no line buffering, no echo, and
// Ctrl-C becomes byte 0x03 sent to the sandbox instead of SIGINT to us
// restore is idempotent, so it is safe to both defer it and call it early
func Raw(f *os.File) (restore func(), err error) {
	fd := int(f.Fd())
	old, err := term.MakeRaw(fd)
	if err != nil {
		return nil, err
	}
	var once sync.Once
	return func() { once.Do(func() { _ = term.Restore(fd, old) }) }, nil
}

// WithRaw runs fn with f in raw mode and always restores the terminal: on
// return, on error, and when fn panics (a deferred call runs while a panic
// unwinds, before the runtime prints the trace, so the trace is readable)
// it deliberately does not recover: re-panicking would lose the stack
// limit: a panic in a goroutine fn started kills the process without
// running any defers; Go offers no hook for that
func WithRaw(f *os.File, fn func() error) error {
	restore, err := Raw(f)
	if err != nil {
		return err
	}
	defer restore()
	return fn()
}

func Size(f *os.File) backend.Size {
	w, h, err := term.GetSize(int(f.Fd()))
	if err != nil {
		return backend.Size{Rows: 24, Cols: 80}
	}
	return backend.Size{Rows: uint16(h), Cols: uint16(w)}
}

// WatchResize emits the new size on every SIGWINCH until ctx ends, then closes
// Coalescing: if the consumer is slow, drop the stale size and keep the newest
func WatchResize(ctx context.Context, f *os.File) <-chan backend.Size {
	out := make(chan backend.Size, 1)
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGWINCH)
	go func() {
		defer close(out)
		defer signal.Stop(sig)
		for {
			select {
			case <-ctx.Done():
				return
			case <-sig:
				select {
				case <-out: // discard stale
				default:
				}
				out <- Size(f)
			}
		}
	}()
	return out
}

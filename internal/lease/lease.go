// Package lease implements "I am still alive" heartbeats as file mtimes.
// A CLI killed with SIGKILL runs no cleanup code, but it also stops touching
// its lease file. The reaper notices the stale mtime and incinerates for it.
package lease

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

const (
	Interval = 5 * time.Second  // how often the owner touches the file
	TTL      = 30 * time.Second // older than this = owner is dead (6 missed beats)
)

// Dir is $XDG_STATE_HOME/ephemeris/leases (default ~/.local/state/...)
func Dir() string {
	base := os.Getenv("XDG_STATE_HOME")
	if base == "" {
		home, _ := os.UserHomeDir()
		base = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(base, "ephemeris", "leases")
}

type Lease struct {
	path string
	stop context.CancelFunc
	done chan struct{}
}

// Hold creates the lease and starts heartbeating. take the lease BEFORE
// creating the sandbox, so there is never a sandbox without an owner record
func Hold(id string) (*Lease, error) {
	if err := os.MkdirAll(Dir(), 0o700); err != nil {
		return nil, err
	}
	p := filepath.Join(Dir(), id)
	if err := os.WriteFile(p, []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	l := &Lease{path: p, stop: cancel, done: make(chan struct{})}
	go func() {
		defer close(l.done)
		t := time.NewTicker(Interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-t.C:
				_ = os.Chtimes(p, now, now)
			}
		}
	}()
	return l, nil
}

// Release is idempotent and waits for the heartbeat goroutine (no leaks)
func (l *Lease) Release() {
	if l == nil {
		return
	}
	l.stop()
	<-l.done
	_ = os.Remove(l.path)
}

func Alive(id string, now time.Time) bool {
	fi, err := os.Stat(filepath.Join(Dir(), id))
	return err == nil && now.Sub(fi.ModTime()) < TTL
}

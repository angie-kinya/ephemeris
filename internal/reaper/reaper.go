// Package reaper finds Ephemeris resources nobody owns any more and destroys them
// it is what makes "incinerate on exit" true even when the exit was a crash
package reaper

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/angie-kinya/ephemeris/internal/backend"
	"github.com/angie-kinya/ephemeris/internal/lease"
)

// Grace protects sandboxes that are being created right now
const Grace = 30 * time.Second

type Result struct {
	Removed []string
	Kept    int
}

// Verdict is pure (no I/O) so it is trivially table-testable
// it returns why a resource should die, or "" to keep it
func Verdict(it backend.Info, host string, now time.Time, alive func(session string) bool) string {
	exp, err := time.Parse(time.RFC3339, it.Labels[backend.LabelExpires])
	if err != nil {
		return "missing or invalid expiry label"
	}
	if now.After(exp) {
		return "ttl expired"
	}
	// Leases are local files. a sandbox started from another machine against a
	// shared engine has no lease here; only its TTL may kill it
	if it.Labels[backend.LabelHost] != host {
		return ""
	}
	if now.Sub(it.Created) < Grace {
		return ""
	}
	if !alive(it.Labels[backend.LabelSession]) {
		return "lease lost: owner crashed or was killed"
	}
	return ""
}

// Reap removes dead resources with bounded concurrency. one failure does not
// stop the others, so it uses a plain errgroup.Group (no shared cancel)
func Reap(ctx context.Context, b backend.Backend, host string, all bool, log *slog.Logger) (Result, error) {
	items, err := b.List(ctx)
	if err != nil {
		return Result{}, err
	}
	now := time.Now()
	alive := func(id string) bool { return lease.Alive(id, now) }

	var (
		mu   sync.Mutex
		res  Result
		errs []error
		g    errgroup.Group
	)
	g.SetLimit(4) // a worker pool in one line: at most 4 removals in flight
	for _, it := range items {
		why := Verdict(it, host, now, alive)
		if all {
			why = "reap --all"
		}
		if why == "" {
			res.Kept++
			continue
		}
		g.Go(func() error {
			err := b.Remove(ctx, it.ID)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, err)
				return nil
			}
			res.Removed = append(res.Removed, it.ID)
			log.Info("reaped", "session", it.Labels[backend.LabelSession], "reason", why)
			return nil
		})
	}
	_ = g.Wait()
	return res, errors.Join(errs...)
}

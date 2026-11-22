// Package pool keeps N sandboxes pre-created and started so a spawn feels instant
// It needs a long-lived process (ephemerisd), because the warm sandboxes
// must outlive any single CLI invocation
package pool

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/angie-kinya/ephemeris/internal/backend"
)

type Factory func(ctx context.Context) (backend.Sandbox, error)

type Pool struct {
	factory Factory
	workers int
	slots   chan struct{}
	ready   chan backend.Sandbox
	log     *slog.Logger
}

func New(f Factory, size, workers int, log *slog.Logger) *Pool {
	return &Pool{
		factory: f, workers: workers, log: log,
		slots: make(chan struct{}, size),
		ready: make(chan backend.Sandbox, size),
	}
}

// Run keeps the pool full until ctx ends, then destroys every idle sandbox
func (p *Pool) Run(ctx context.Context) {
	var wg sync.WaitGroup
	for range p.workers {
		wg.Go(func() {
			backoff := time.Second
			for {
				select {
				case p.slots <- struct{}{}: // blocks while the pool is full: that is the throttle
				case <-ctx.Done():
					return
				}
				sb, err := p.factory(ctx)
				if err != nil {
					<-p.slots // give the token back
					p.log.Warn("warm sandbox failed", "err", err, "retry_in", backoff)
					select {
					case <-time.After(backoff):
						backoff = min(backoff*2, time.Minute)
					case <-ctx.Done():
						return
					}
					continue
				}
				backoff = time.Second
				p.ready <- sb
			}
		})
	}
	wg.Wait()
	p.drain()
}

// Get returns a warm sandbox if one is ready, else builds one cold
func (p *Pool) Get(ctx context.Context) (sb backend.Sandbox, warm bool, err error) {
	select {
	case sb := <-p.ready:
		<-p.slots // frees a token: a worker starts building the replacement
		return sb, true, nil
	default:
		sb, err := p.factory(ctx)
		return sb, false, err
	}
}

func (p *Pool) drain() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for {
		select {
		case sb := <-p.ready:
			<-p.slots
			if err := sb.Destroy(ctx); err != nil {
				p.log.Error("drain", "id", sb.ID(), "err", err)
			}
		default:
			return
		}
	}
}

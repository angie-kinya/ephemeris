package pool

import (
	"context"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/angie-kinya/ephemeris/internal/backend"
)

type fake struct {
	backend.Sandbox
	destroyed *atomic.Int32
}

func (f fake) ID() string                    { return "fake" }
func (f fake) Destroy(context.Context) error { f.destroyed.Add(1); return nil }

func TestPoolFillsServesAndDrains(t *testing.T) {
	defer goleak.VerifyNone(t)
	var built, destroyed atomic.Int32
	factory := func(context.Context) (backend.Sandbox, error) {
		built.Add(1)
		return fake{destroyed: &destroyed}, nil
	}
	p := New(factory, 3, 2, slog.Default())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { p.Run(ctx); close(done) }()

	waitFor(t, func() bool { return len(p.ready) == 3 })
	if _, warm, _ := p.Get(ctx); !warm {
		t.Fatal("expected a warm sandbox")
	}
	waitFor(t, func() bool { return built.Load() == 4 }) // replacement built

	cancel()
	<-done
	if destroyed.Load() != 3 {
		t.Fatalf("drain destroyed %d, want 3", destroyed.Load())
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatal("condition not reached")
}

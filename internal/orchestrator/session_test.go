package orchestrator

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/angie-kinya/ephemeris/internal/backend"
	"github.com/angie-kinya/ephemeris/internal/backend/fake"
	"github.com/angie-kinya/ephemeris/internal/spec"
)

func spawnFake(t *testing.T, b *fake.Backend) *Session {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", t.TempDir()) // leases go here
	o := &Orchestrator{Backend: b, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Host: "test"}
	sess, err := o.Spawn(context.Background(), spec.Default())
	if err != nil {
		t.Fatal(err)
	}
	return sess
}

// run with -race: N execs overlap inside the sandbox at the same moment
func TestConcurrentExec(t *testing.T) {
	const n = 8
	var (
		inside  atomic.Int32
		allIn   = make(chan struct{})
		release = make(chan struct{})
	)
	b := &fake.Backend{Exec: func(ctx context.Context, _ backend.ExecRequest) (int, error) {
		if inside.Add(1) == n {
			close(allIn)
		}
		<-release
		return 7, nil
	}}
	sess := spawnFake(t, b)
	defer sess.Close(context.Background())

	var wg sync.WaitGroup
	errs := make(chan error, n)
	for range n {
		wg.Go(func() {
			code, err := sess.Exec(context.Background(), backend.ExecRequest{Cmd: []string{"x"}})
			if err == nil && code != 7 {
				t.Errorf("exit code %d, want 7", code)
			}
			errs <- err
		})
	}
	<-allIn // every exec is running at once: none was refused
	if got := sess.State(); got != Attached {
		t.Fatalf("state with %d running execs = %s, want attached", n, got)
	}
	close(release)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if got := sess.State(); got != Ready {
		t.Fatalf("state after all execs = %s, want ready", got)
	}
}

// Close while execs run: the session must stay Destroying, and new execs fail
func TestCloseDuringExec(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	b := &fake.Backend{Exec: func(context.Context, backend.ExecRequest) (int, error) {
		close(started)
		<-release
		return 0, nil
	}}
	sess := spawnFake(t, b)
	done := make(chan error)
	go func() {
		_, err := sess.Exec(context.Background(), backend.ExecRequest{})
		done <- err
	}()
	<-started
	if _, err := sess.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := sess.Exec(context.Background(), backend.ExecRequest{}); err == nil {
		t.Fatal("exec after close succeeded")
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if got := sess.State(); got != Destroyed {
		t.Fatalf("state = %s, want destroyed", got)
	}
	if b.Len() != 0 {
		t.Fatal("sandbox survived Close")
	}
}

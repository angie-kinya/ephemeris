// Package fake is an in-memory backend.Backend for tests: no engine, no
// namespaces. Exec runs a Go function instead of a process
package fake

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/angie-kinya/ephemeris/internal/backend"
	"github.com/angie-kinya/ephemeris/internal/spec"
)

// ExecFunc stands in for the sandboxed process
type ExecFunc func(ctx context.Context, req backend.ExecRequest) (int, error)

type Backend struct {
	// Exec is used by every sandbox this backend creates. nil prints req.Cmd
	// (space-joined) to stdout and exits 0
	Exec ExecFunc

	mu    sync.Mutex
	boxes map[string]backend.Info
}

func (b *Backend) Name() string { return "fake" }

func (b *Backend) Create(_ context.Context, _ spec.Spec, labels map[string]string) (backend.Sandbox, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.boxes == nil {
		b.boxes = map[string]backend.Info{}
	}
	id := "fake-" + labels[backend.LabelSession]
	b.boxes[id] = backend.Info{ID: id, Labels: maps.Clone(labels), Created: time.Now()}
	return &sandbox{b: b, id: id}, nil
}

func (b *Backend) List(context.Context) ([]backend.Info, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return slices.Collect(maps.Values(b.boxes)), nil
}

func (b *Backend) Remove(_ context.Context, id string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.boxes, id)
	return nil
}

// Len is the number of sandboxes that exist right now
func (b *Backend) Len() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.boxes)
}

type sandbox struct {
	b  *Backend
	id string
}

func (s *sandbox) ID() string                        { return s.id }
func (s *sandbox) Start(context.Context) error       { return nil }
func (s *sandbox) Destroy(ctx context.Context) error { return s.b.Remove(ctx, s.id) }

func (s *sandbox) Exec(ctx context.Context, req backend.ExecRequest) (int, error) {
	if s.b.Exec != nil {
		return s.b.Exec(ctx, req)
	}
	_, err := fmt.Fprintln(backend.OrDiscard(req.Stdout), strings.Join(req.Cmd, " "))
	return 0, err
}

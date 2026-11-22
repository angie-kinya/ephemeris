package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/angie-kinya/ephemeris/internal/backend"
	"github.com/angie-kinya/ephemeris/internal/backend/docker"
	"github.com/angie-kinya/ephemeris/internal/backend/process"
	"github.com/angie-kinya/ephemeris/internal/lease"
	"github.com/angie-kinya/ephemeris/internal/orchestrator"
	"github.com/angie-kinya/ephemeris/internal/provision"
	"github.com/angie-kinya/ephemeris/internal/reaper"
	"github.com/angie-kinya/ephemeris/internal/receipt"
	"github.com/angie-kinya/ephemeris/internal/spec"
)

type globals struct {
	specPath string
	verbose  bool
}

// app is everything a command needs, wired once. no package-level globals
type app struct {
	log     *slog.Logger
	spec    spec.Spec
	backend backend.Backend
	host    string

	mu    sync.Mutex
	extra map[string]backend.Backend // other backends opened on demand
}

func (g *globals) setup(ctx context.Context) (*app, error) {
	level := slog.LevelInfo
	if g.verbose {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	s, err := loadSpec(g.specPath)
	if err != nil {
		return nil, err
	}
	b, err := newBackend(ctx, s.Backend)
	if err != nil {
		return nil, err
	}
	host, _ := os.Hostname()
	return &app{log: log, spec: s, backend: b, host: host}, nil
}

// loadSpec: -f wins; else ./ephemeris.yaml; else defaults, shipping the
// current repo if we are inside one. zero config is the onboarding story
func loadSpec(path string) (spec.Spec, error) {
	if path == "" {
		if _, err := os.Stat("ephemeris.yaml"); err == nil {
			path = "ephemeris.yaml"
		}
	}
	if path != "" {
		return spec.Load(path)
	}
	s := spec.Default()
	if _, err := os.Stat(".git"); err == nil {
		s.Source.Path = "."
	}
	return s, s.Validate()
}

func newBackend(ctx context.Context, name string) (backend.Backend, error) {
	switch name {
	case "docker":
		return docker.New(ctx)
	case "process":
		return process.New()
	}
	return nil, fmt.Errorf("unknown backend %q", name)
}

// withSession is the lifecycle every command shares:
// reap leftovers (in the background) -> spawn -> run fn -> incinerate -> signed receipt
func (a *app) withSession(ctx context.Context, fn func(context.Context, *orchestrator.Session) (int, error)) (int, error) {
	// the reap runs alongside the spawn instead of before it. it cannot take
	// the new sandbox with it: the lease is held before the sandbox exists,
	// and reaper.Verdict keeps anything on this host younger than reaper.Grace
	waitReap := a.reapInBackground(ctx)
	defer waitReap()

	sess, err := a.spawn(ctx, a.spec)
	if err != nil {
		return 1, err
	}
	defer func() { _, _ = a.destroy(ctx, sess) }()

	tctx, cancelTTL := sess.WithTTL(ctx)
	defer cancelTTL()
	code, err := fn(tctx, sess)
	if errors.Is(err, context.DeadlineExceeded) {
		a.log.Warn("session hit its ttl", "ttl", a.spec.TTL)
	}
	return code, err
}

// reapInBackground cleans up after earlier crashed sessions without delaying
// the caller. the returned func waits for it (bounded by its own 10s budget)
func (a *app) reapInBackground(ctx context.Context) (wait func()) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		if res, err := reaper.Reap(rctx, a.backend, a.host, false, a.log); err != nil {
			a.log.Warn("reaper", "err", err)
		} else if len(res.Removed) > 0 {
			a.log.Info("cleaned up after earlier crashed sessions", "count", len(res.Removed))
		}
	}()
	return func() { <-done }
}

// backendFor returns the app's backend, or lazily opens (and caches) another
// one when a spec asks for a different backend (the MCP server does this)
func (a *app) backendFor(ctx context.Context, name string) (backend.Backend, error) {
	if name == a.backend.Name() {
		return a.backend, nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if b, ok := a.extra[name]; ok {
		return b, nil
	}
	b, err := newBackend(ctx, name)
	if err != nil {
		return nil, err
	}
	if a.extra == nil {
		a.extra = map[string]backend.Backend{}
	}
	a.extra[name] = b
	return b, nil
}

// spawn creates, starts and provisions one sandbox from s
func (a *app) spawn(ctx context.Context, s spec.Spec) (*orchestrator.Session, error) {
	b, err := a.backendFor(ctx, s.Backend)
	if err != nil {
		return nil, err
	}
	o := &orchestrator.Orchestrator{
		Backend:      b,
		Provisioners: []orchestrator.Provisioner{provision.Source{}, provision.Secrets{}},
		Log:          a.log,
		Host:         a.host,
	}
	start := time.Now()
	sess, err := o.Spawn(ctx, s)
	if err != nil {
		return nil, err
	}
	a.log.Info("sandbox ready", "session", sess.ID, "backend", b.Name(), "took", time.Since(start).Round(time.Millisecond), "expires", sess.Expires.Local().Format(time.Kitchen))
	return sess, nil
}

// destroy incinerates sess and returns its signed receipt, which is also
// saved to disk. a teardown failure leaves the sandbox to the reaper
func (a *app) destroy(ctx context.Context, sess *orchestrator.Session) (receipt.Receipt, error) {
	r, err := sess.Close(ctx)
	if err != nil {
		a.log.Error("teardown failed; the reaper will retry", "session", sess.ID, "err", err)
		return r, err
	}
	signed, p, err := a.saveReceipt(r)
	if err != nil {
		a.log.Warn("receipt", "err", err)
		return r, err
	}
	a.log.Info("incinerated", "session", sess.ID, "receipt", p)
	return signed, nil
}

func stateDir() string { return filepath.Dir(lease.Dir()) }

func keyPath() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		dir = stateDir()
	}
	return filepath.Join(dir, "ephemeris", "receipt.key")
}

func (a *app) saveReceipt(r receipt.Receipt) (receipt.Receipt, string, error) {
	key, err := receipt.LoadOrCreateKey(keyPath())
	if err != nil {
		return r, "", err
	}
	if r, err = receipt.Sign(r, key); err != nil {
		return r, "", err
	}
	p, err := receipt.Save(filepath.Join(stateDir(), "receipts"), r)
	return r, p, err
}

func exists(p string) bool {
	_, err := os.Stat(p)
	return !errors.Is(err, fs.ErrNotExist)
}

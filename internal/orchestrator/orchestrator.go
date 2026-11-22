// Package orchestrator turns a Spec into a running, provisioned Session
package orchestrator

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/angie-kinya/ephemeris/internal/backend"
	"github.com/angie-kinya/ephemeris/internal/lease"
	"github.com/angie-kinya/ephemeris/internal/spec"
)

// Provisioner puts one thing into a started sandbox: source code, secrets,
// later a database. new features are new Provisioners, not edits to Spawn
type Provisioner interface {
	Name() string
	Provision(ctx context.Context, sb backend.Sandbox, s spec.Spec) error
}

type Orchestrator struct {
	Backend      backend.Backend
	Provisioners []Provisioner
	Log          *slog.Logger
	Host         string
}

func (o *Orchestrator) Spawn(ctx context.Context, s spec.Spec) (_ *Session, err error) {
	sess := newSession(s, o.Backend.Name(), o.Host)
	if sess.lease, err = lease.Hold(sess.ID); err != nil {
		return nil, fmt.Errorf("take lease: %w", err)
	}

	defer func() {
		if err != nil {
			if _, cerr := sess.Close(ctx); cerr != nil {
				o.Log.Error("cleanup after failed spawn", "session", sess.ID, "err", cerr)
			}
		}
	}()
	if err = sess.transition(Provisioning); err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(ctx, s.ProvisionTimeout)
	defer cancel()

	labels := map[string]string{
		backend.LabelManaged: "true",
		backend.LabelSession: sess.ID,
		backend.LabelExpires: sess.Expires.Format(time.RFC3339),
		backend.LabelHost:    o.Host,
	}

	if sess.sandbox, err = o.Backend.Create(ctx, s, labels); err != nil {
		return nil, fmt.Errorf("create sandbox: %w", err)
	}
	if err = sess.sandbox.Start(ctx); err != nil {
		return nil, fmt.Errorf("start sandbox: %w", err)
	}

	g, gctx := errgroup.WithContext(ctx)
	for _, p := range o.Provisioners {
		g.Go(func() error {
			t := time.Now()
			if err := p.Provision(gctx, sess.sandbox, s); err != nil {
				return fmt.Errorf("provision %s: %w", p.Name(), err)
			}
			o.Log.Debug("provisioned", "step", p.Name(), "took", time.Since(t).Round(time.Millisecond))
			return nil
		})
	}
	if err = g.Wait(); err != nil {
		return nil, err
	}
	if err = sess.transition(Ready); err != nil {
		return nil, err
	}
	return sess, nil
}

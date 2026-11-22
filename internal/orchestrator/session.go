package orchestrator

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sync"
	"time"

	"github.com/angie-kinya/ephemeris/internal/backend"
	"github.com/angie-kinya/ephemeris/internal/lease"
	"github.com/angie-kinya/ephemeris/internal/receipt"
	"github.com/angie-kinya/ephemeris/internal/spec"
)

type State int

const (
	Requested State = iota
	Provisioning
	Ready
	Attached
	Destroying
	Destroyed
)

func (s State) String() string {
	return [...]string{"requested", "provisioning", "ready", "attached", "destroying", "destroyed"}[s]
}

// next is the whole state machine. every state may jump to Destroying; nothing leaves Destroyed
// Attached means "at least one exec is running": the first exec moves
// Ready -> Attached, the last one to finish moves Attached -> Ready, and
// execs that overlap them do not transition at all (see Session.execs)
var next = map[State][]State{
	Requested:    {Provisioning, Destroying},
	Provisioning: {Ready, Destroying},
	Ready:        {Attached, Destroying},
	Attached:     {Ready, Destroying},
	Destroying:   {Destroyed},
}

type Session struct {
	ID      string
	Spec    spec.Spec
	Backend string
	Host    string
	Created time.Time
	Expires time.Time

	mu      sync.Mutex
	state   State
	execs   int // running Execs, state is attached exactly while execs > 0
	sandbox backend.Sandbox
	lease   *lease.Lease

	closeOnce sync.Once
	receipt   receipt.Receipt
	closeErr  error
}

func newSession(s spec.Spec, backendName, host string) *Session {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	now := time.Now().UTC()
	return &Session{
		ID: hex.EncodeToString(b), Spec: s, Backend: backendName, Host: host,
		Created: now, Expires: now.Add(s.TTL),
	}
}

func (s *Session) State() State {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state
}

func (s *Session) transition(to State) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.transitionLocked(to)
}

// transitionLocked is transition for callers that already hold s.mu
func (s *Session) transitionLocked(to State) error {
	for _, ok := range next[s.state] {
		if ok == to {
			s.state = to
			return nil
		}
	}
	return fmt.Errorf("session %s: illegal transition %s -> %s", s.ID, s.state, to)
}

// WithTTL bounds anything you run in the session by its hard expiry
// the reaper enforces the same deadline from outside via the expires label
func (s *Session) WithTTL(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithDeadline(ctx, s.Expires)
}

// Exec runs one command in the sandbox. it is safe to call concurrently:
// every exec holds a reference, and the session is Attached while any do
func (s *Session) Exec(ctx context.Context, req backend.ExecRequest) (int, error) {
	if err := s.attach(); err != nil {
		return -1, err
	}
	defer s.detach()
	return s.sandbox.Exec(ctx, req)
}

func (s *Session) attach() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.execs == 0 {
		if err := s.transitionLocked(Attached); err != nil {
			return err
		}
	}
	// execs > 0 implies Attached; anything else (Destroying) was rejected above
	// for the first exec, and later ones must be rejected too
	if s.state != Attached {
		return fmt.Errorf("session %s: cannot exec while %s", s.ID, s.state)
	}
	s.execs++
	return nil
}

func (s *Session) detach() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.execs--
	// if Close ran meanwhile the session is Destroying; it must not go back
	if s.execs == 0 && s.state == Attached {
		_ = s.transitionLocked(Ready)
	}
}

// Close incinerates the session. safe to call many times and from defers
func (s *Session) Close(ctx context.Context) (receipt.Receipt, error) {
	s.closeOnce.Do(func() {
		_ = s.transition(Destroying)
		// ctx may already be cancelled
		// cleanup gets its own budget, detached from the caller's cancellation
		dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()

		r := receipt.Receipt{
			Session: s.ID, Backend: s.Backend, Image: s.Spec.Image, Host: s.Host,
			Created: s.Created,
		}
		if s.sandbox != nil {
			r.Resource = s.sandbox.ID()
			s.closeErr = s.sandbox.Destroy(dctx)
		}
		r.Destroyed = time.Now().UTC()
		r.Verified = s.closeErr == nil
		// released even on failure: a lease-less sandbox is the reaper's job now
		s.lease.Release()
		if s.closeErr == nil {
			_ = s.transition(Destroyed)
		}
		s.receipt = r
	})
	return s.receipt, s.closeErr
}

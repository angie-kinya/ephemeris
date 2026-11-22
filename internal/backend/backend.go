// Package backend is the seam between "what a sandbox is" and "how one is made".
// Docker, bubblewrap, gVisor and Firecracker all hide behind these two interfaces.
package backend

import (
	"context"
	"io"
	"time"

	"github.com/angie-kinya/ephemeris/internal/spec"
)

// labels are written onto every resource. they are how the reaper finds
// leftovers after a crash: the engine remembers them even if we die.
const (
	LabelManaged = "dev.ephemeris.managed"
	LabelSession = "dev.ephemeris.session"
	LabelExpires = "dev.ephemeris.expires" // RFC 3339
	LabelHost    = "dev.ephemeris.host"
)

type Size struct{ Rows, Cols uint16 }

type ExecRequest struct {
	Cmd         []string
	Env         []string
	WorkDir     string
	TTY         bool
	InitialSize Size
	Resize      <-chan Size // closed by the caller when done
	Stdin       io.Reader
	Stdout      io.Writer
	Stderr      io.Writer
}

type Info struct {
	ID      string
	Labels  map[string]string
	Created time.Time
}

type Backend interface {
	Name() string
	Create(ctx context.Context, s spec.Spec, labels map[string]string) (Sandbox, error)
	List(ctx context.Context) ([]Info, error)
	Remove(ctx context.Context, id string) error
}

type Sandbox interface {
	ID() string
	Start(ctx context.Context) error
	Exec(ctx context.Context, req ExecRequest) (exitCode int, err error)
	Destroy(ctx context.Context) error
}

func OrDiscard(w io.Writer) io.Writer {
	if w == nil {
		return io.Discard
	}
	return w
}

// Package mcpserver exposes Ephemeris sandboxes to AI agents as MCP tools.
//
// It owns sandbox lifetimes for one MCP connection: every session it creates
// is destroyed when the session's TTL runs out, when the agent asks, or when
// the server shuts down, whichever comes first. Spawning and teardown are
// injected (Config.Spawn, Config.Destroy) so the CLI's wiring is reused and
// tests can run on a fake backend
package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/angie-kinya/ephemeris/internal/backend"
	"github.com/angie-kinya/ephemeris/internal/orchestrator"
	"github.com/angie-kinya/ephemeris/internal/receipt"
)

const (
	DefaultMaxSessions = 4
	OutputCap          = 64 << 10 // per stream, per exec
	DefaultExecTimeout = 2 * time.Minute
	MaxExecTimeout     = time.Hour
)

type Config struct {
	// Spawn creates a ready session. specPath "" means the server's default
	// spec; ttl 0 means the spec's own TTL
	Spawn func(ctx context.Context, specPath string, ttl time.Duration) (*orchestrator.Session, error)
	// Destroy incinerates a session and returns its signed receipt
	Destroy     func(ctx context.Context, s *orchestrator.Session) (receipt.Receipt, error)
	Log         *slog.Logger
	MaxSessions int // 0 = DefaultMaxSessions
	// AllowSpecPath lets clients pick a spec file. Off by default: a spec's
	// secrets (from: file:...) can read any host file into the sandbox, so an
	// agent choosing specs could exfiltrate your files
	AllowSpecPath bool
	Version       string // reported to clients
}

type Server struct {
	cfg Config
	srv *mcp.Server

	mu       sync.Mutex
	sessions map[string]*entry
	pending  int  // creations in flight; they count against MaxSessions
	closed   bool // set by Shutdown; refuses new sessions
	bg       sync.WaitGroup
}

type entry struct {
	sess  *orchestrator.Session
	timer *time.Timer // destroys the session at its TTL
}

func New(cfg Config) *Server {
	if cfg.MaxSessions <= 0 {
		cfg.MaxSessions = DefaultMaxSessions
	}
	if cfg.Log == nil {
		cfg.Log = slog.New(slog.DiscardHandler)
	}
	if cfg.Version == "" {
		cfg.Version = "dev"
	}
	s := &Server{cfg: cfg, sessions: map[string]*entry{}}
	s.srv = mcp.NewServer(&mcp.Implementation{Name: "ephemeris", Title: "Ephemeris sandboxes", Version: cfg.Version}, &mcp.ServerOptions{
		Instructions: "Ephemeris runs commands in disposable, locked-down sandboxes (no network unless the spec allows it). " +
			"Create one with sandbox_create, run commands with sandbox_exec, and call sandbox_destroy when done to get a signed teardown receipt. " +
			fmt.Sprintf("At most %d sandboxes may exist at once; each also dies at its TTL.", cfg.MaxSessions),
	})
	s.register()
	return s
}

// MCP is the underlying SDK server, e.g. to connect an in-memory transport
func (s *Server) MCP() *mcp.Server { return s.srv }

// Run serves one client over t until it disconnects (stdin EOF for stdio) or
// ctx ends, then destroys every sandbox it still owns
func (s *Server) Run(ctx context.Context, t mcp.Transport) error {
	err := s.srv.Run(ctx, t)
	s.Shutdown(context.WithoutCancel(ctx))
	if errors.Is(err, context.Canceled) && ctx.Err() != nil {
		return nil // a signal is a normal way to stop
	}
	return err
}

// Shutdown refuses new sessions and destroys all existing ones in parallel
// creations still in flight destroy their own session when they finish
func (s *Server) Shutdown(ctx context.Context) {
	s.mu.Lock()
	s.closed = true
	all := make([]*entry, 0, len(s.sessions))
	for id, e := range s.sessions {
		e.timer.Stop()
		all = append(all, e)
		delete(s.sessions, id)
	}
	s.mu.Unlock()

	var wg sync.WaitGroup
	for _, e := range all {
		wg.Go(func() { _, _ = s.cfg.Destroy(ctx, e.sess) })
	}
	wg.Wait()
	s.bg.Wait() // TTL expiries and late creations
	if len(all) > 0 {
		s.cfg.Log.Info("mcp: destroyed all sandboxes on shutdown", "count", len(all))
	}
}

// take removes a session from the map; the caller now owns its destruction
func (s *Server) take(id string) (*orchestrator.Session, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.sessions[id]
	if !ok {
		return nil, false
	}
	e.timer.Stop()
	delete(s.sessions, id)
	return e.sess, true
}

func (s *Server) get(id string) (*orchestrator.Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.sessions[id]
	if !ok {
		return nil, fmt.Errorf("no sandbox %q: it was destroyed, expired, or never existed (see sandbox_list)", id)
	}
	return e.sess, nil
}

// tool schemas

type CreateInput struct {
	SpecPath string `json:"spec_path,omitempty" jsonschema:"path to an ephemeris.yaml spec on the server's machine; default is the server's spec"`
	TTL      string `json:"ttl,omitempty" jsonschema:"lifetime as a Go duration such as 10m or 1h (1m to 24h); default is the spec's ttl"`
}

type CreateOutput struct {
	SessionID string    `json:"session_id"`
	ExpiresAt time.Time `json:"expires_at"`
}

type ExecInput struct {
	SessionID      string   `json:"session_id" jsonschema:"id returned by sandbox_create"`
	Command        []string `json:"command" jsonschema:"argv to run, e.g. [\"sh\",\"-c\",\"ls -la\"]; no shell is implied"`
	Stdin          string   `json:"stdin,omitempty" jsonschema:"data written to the command's stdin, then closed"`
	TimeoutSeconds int      `json:"timeout_seconds,omitempty" jsonschema:"kill the call after this many seconds (default 120, max 3600)"`
}

type ExecOutput struct {
	ExitCode  int    `json:"exit_code"`
	Stdout    string `json:"stdout"`
	Stderr    string `json:"stderr"`
	Truncated bool   `json:"truncated" jsonschema:"true if stdout or stderr exceeded 64 KiB and was cut"`
	TimedOut  bool   `json:"timed_out,omitempty"`
}

type SessionInput struct {
	SessionID string `json:"session_id" jsonschema:"id returned by sandbox_create"`
}

type ListOutput struct {
	Sandboxes []SandboxInfo `json:"sandboxes"`
}

type SandboxInfo struct {
	SessionID string    `json:"session_id"`
	Backend   string    `json:"backend"`
	Image     string    `json:"image,omitempty"`
	State     string    `json:"state"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
}

func (s *Server) register() {
	yes := true
	mcp.AddTool(s.srv, &mcp.Tool{
		Name:        "sandbox_create",
		Description: "Create a fresh, isolated sandbox and return its session_id. The project source (if the spec ships one) is at /workspace.",
	}, s.create)
	mcp.AddTool(s.srv, &mcp.Tool{
		Name:        "sandbox_exec",
		Description: "Run a command in a sandbox and return its exit code and output (each stream capped at 64 KiB). Runs in /workspace.",
	}, s.exec)
	mcp.AddTool(s.srv, &mcp.Tool{
		Name:         "sandbox_destroy",
		Description:  "Destroy a sandbox and return the signed teardown receipt proving it is gone (verify it later with `ephemeris verify`).",
		Annotations:  &mcp.ToolAnnotations{DestructiveHint: &yes},
		OutputSchema: receiptSchema(),
	}, s.destroy)
	mcp.AddTool(s.srv, &mcp.Tool{
		Name:        "sandbox_list",
		Description: "List the sandboxes this server currently owns.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, s.list)
}

func (s *Server) create(ctx context.Context, _ *mcp.CallToolRequest, in CreateInput) (*mcp.CallToolResult, CreateOutput, error) {
	var ttl time.Duration
	if in.TTL != "" {
		d, err := time.ParseDuration(in.TTL)
		if err != nil {
			return nil, CreateOutput{}, fmt.Errorf("ttl %q: %w", in.TTL, err)
		}
		ttl = d
	}

	s.mu.Lock()
	switch {
	case s.closed:
		s.mu.Unlock()
		return nil, CreateOutput{}, errors.New("server is shutting down")
	case len(s.sessions)+s.pending >= s.cfg.MaxSessions:
		s.mu.Unlock()
		return nil, CreateOutput{}, fmt.Errorf("limit of %d concurrent sandboxes reached; destroy one first", s.cfg.MaxSessions)
	}
	s.pending++
	s.bg.Add(1)
	s.mu.Unlock()
	defer s.bg.Done()

	var sess *orchestrator.Session
	var err error
	if in.SpecPath != "" && !s.cfg.AllowSpecPath {
		err = fmt.Errorf("spec_path is disabled; restart the server with --allow-spec-path to allow it")
	} else {
		sess, err = s.cfg.Spawn(ctx, in.SpecPath, ttl)
	}

	s.mu.Lock()
	s.pending--
	if err != nil {
		s.mu.Unlock()
		return nil, CreateOutput{}, err
	}
	if s.closed {
		s.mu.Unlock()
		_, _ = s.cfg.Destroy(context.WithoutCancel(ctx), sess)
		return nil, CreateOutput{}, errors.New("server is shutting down")
	}
	id := sess.ID
	s.sessions[id] = &entry{sess: sess, timer: time.AfterFunc(time.Until(sess.Expires), func() { s.expire(id) })}
	s.mu.Unlock()

	s.cfg.Log.Info("mcp: sandbox created", "session", id, "expires", sess.Expires)
	return nil, CreateOutput{SessionID: id, ExpiresAt: sess.Expires}, nil
}

func (s *Server) expire(id string) {
	s.mu.Lock()
	e, ok := s.sessions[id]
	if !ok || s.closed {
		s.mu.Unlock()
		return
	}
	delete(s.sessions, id)
	s.bg.Add(1)
	s.mu.Unlock()
	defer s.bg.Done()
	s.cfg.Log.Info("mcp: sandbox reached its ttl", "session", id)
	_, _ = s.cfg.Destroy(context.Background(), e.sess)
}

func (s *Server) exec(ctx context.Context, _ *mcp.CallToolRequest, in ExecInput) (*mcp.CallToolResult, ExecOutput, error) {
	if len(in.Command) == 0 || in.Command[0] == "" {
		return nil, ExecOutput{}, errors.New("command must be a non-empty argv array")
	}
	timeout := DefaultExecTimeout
	if in.TimeoutSeconds < 0 {
		return nil, ExecOutput{}, errors.New("timeout_seconds must be positive")
	}
	if in.TimeoutSeconds > 0 {
		timeout = min(time.Duration(in.TimeoutSeconds)*time.Second, MaxExecTimeout)
	}
	sess, err := s.get(in.SessionID)
	if err != nil {
		return nil, ExecOutput{}, err
	}

	ctx, cancel := sess.WithTTL(ctx)
	defer cancel()
	ctx, cancelTimeout := context.WithTimeout(ctx, timeout)
	defer cancelTimeout()

	stdout, stderr := &capped{max: OutputCap}, &capped{max: OutputCap}
	req := backend.ExecRequest{Cmd: in.Command, WorkDir: "/workspace", Stdout: stdout, Stderr: stderr}
	if in.Stdin != "" {
		req.Stdin = strings.NewReader(in.Stdin)
	}
	code, err := sess.Exec(ctx, req)
	out := ExecOutput{
		ExitCode: code, Stdout: stdout.String(), Stderr: stderr.String(),
		Truncated: stdout.truncated() || stderr.truncated(),
	}
	if err != nil {
		if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, ExecOutput{}, err
		}
		// a timeout is a result, not a failure, keep the partial output
		why := fmt.Sprintf("timeout_seconds (%s) elapsed", timeout)
		if !time.Now().Before(sess.Expires) {
			why = "the sandbox reached its ttl"
		}
		out.ExitCode, out.TimedOut = -1, true
		out.Stderr += "\n[ephemeris: command stopped: " + why + "]"
	}
	return nil, out, nil
}

func (s *Server) destroy(ctx context.Context, _ *mcp.CallToolRequest, in SessionInput) (*mcp.CallToolResult, receipt.Receipt, error) {
	sess, ok := s.take(in.SessionID)
	if !ok {
		_, err := s.get(in.SessionID)
		return nil, receipt.Receipt{}, err
	}
	r, err := s.cfg.Destroy(context.WithoutCancel(ctx), sess)
	if err != nil {
		return nil, receipt.Receipt{}, fmt.Errorf("destroy %s: %w (the reaper will retry)", in.SessionID, err)
	}
	return nil, r, nil
}

func (s *Server) list(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, ListOutput, error) {
	s.mu.Lock()
	out := ListOutput{Sandboxes: []SandboxInfo{}}
	for _, e := range s.sessions {
		x := e.sess
		out.Sandboxes = append(out.Sandboxes, SandboxInfo{
			SessionID: x.ID, Backend: x.Backend, Image: x.Spec.Image, State: x.State().String(),
			CreatedAt: x.Created, ExpiresAt: x.Expires,
		})
	}
	s.mu.Unlock()
	slices.SortFunc(out.Sandboxes, func(a, b SandboxInfo) int { return a.CreatedAt.Compare(b.CreatedAt) })
	return nil, out, nil
}

// receiptSchema: the inferred schema says []byte is an array, but
// encoding/json writes it as a base64 string, so the SDK's output validation
// would reject every real receipt
func receiptSchema() *jsonschema.Schema {
	sch, err := jsonschema.For[receipt.Receipt](&jsonschema.ForOptions{
		TypeSchemas: map[reflect.Type]*jsonschema.Schema{
			reflect.TypeFor[[]byte](): {Type: "string", Description: "base64 ed25519 signature"},
		},
	})
	if err != nil {
		panic(err) // a static type, fails on every run or never
	}
	return sch
}

// capped keeps the first max bytes and silently drops the rest. it never
// returns an error, failing the write would kill the sandboxed command
// with SIGPIPE-like behaviour and change what it does
type capped struct {
	mu      sync.Mutex
	buf     []byte
	max     int
	dropped int
}

func (c *capped) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	room := c.max - len(c.buf)
	if room >= len(p) {
		c.buf = append(c.buf, p...)
	} else {
		c.buf = append(c.buf, p[:max(room, 0)]...)
		c.dropped += len(p) - max(room, 0)
	}
	return len(p), nil
}

func (c *capped) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return string(c.buf)
}

func (c *capped) truncated() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.dropped > 0
}

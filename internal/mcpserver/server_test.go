package mcpserver

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"io"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/angie-kinya/ephemeris/internal/backend"
	"github.com/angie-kinya/ephemeris/internal/backend/fake"
	"github.com/angie-kinya/ephemeris/internal/orchestrator"
	"github.com/angie-kinya/ephemeris/internal/receipt"
	"github.com/angie-kinya/ephemeris/internal/spec"
)

type harness struct {
	t      *testing.T
	b      *fake.Backend
	srv    *Server
	client *mcp.ClientSession
	pub    ed25519.PublicKey
	done   chan error
	cancel context.CancelFunc
}

func start(t *testing.T, exec fake.ExecFunc) *harness {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", t.TempDir()) // leases
	pub, key, _ := ed25519.GenerateKey(nil)
	b := &fake.Backend{Exec: exec}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	o := &orchestrator.Orchestrator{Backend: b, Log: log, Host: "test"}
	srv := New(Config{
		Log: log,
		Spawn: func(ctx context.Context, specPath string, ttl time.Duration) (*orchestrator.Session, error) {
			s := spec.Default()
			if ttl > 0 {
				s.TTL = ttl
			}
			if err := s.Validate(); err != nil {
				return nil, err
			}
			return o.Spawn(ctx, s)
		},
		Destroy: func(ctx context.Context, s *orchestrator.Session) (receipt.Receipt, error) {
			r, err := s.Close(ctx)
			if err != nil {
				return r, err
			}
			return receipt.Sign(r, key)
		},
	})

	ctx, cancel := context.WithCancel(context.Background())
	st, ct := mcp.NewInMemoryTransports()
	h := &harness{t: t, b: b, srv: srv, pub: pub, done: make(chan error, 1), cancel: cancel}
	go func() { h.done <- srv.Run(ctx, st) }()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "v0"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	h.client = cs
	t.Cleanup(func() { cancel(); _ = cs.Close() })
	return h
}

// call invokes a tool and decodes its structured result into out
// it returns the tool error text, or "" on success
func (h *harness) call(name string, args, out any) string {
	h.t.Helper()
	res, err := h.client.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		h.t.Fatalf("%s: protocol error: %v", name, err)
	}
	if res.IsError {
		var sb strings.Builder
		for _, c := range res.Content {
			if tc, ok := c.(*mcp.TextContent); ok {
				sb.WriteString(tc.Text)
			}
		}
		return sb.String()
	}
	raw, _ := json.Marshal(res.StructuredContent)
	if err := json.Unmarshal(raw, out); err != nil {
		h.t.Fatalf("%s: decode %s: %v", name, raw, err)
	}
	return ""
}

func (h *harness) create() string {
	h.t.Helper()
	var out CreateOutput
	if e := h.call("sandbox_create", map[string]any{"ttl": "5m"}, &out); e != "" {
		h.t.Fatal("create:", e)
	}
	if out.SessionID == "" || time.Until(out.ExpiresAt) < 4*time.Minute {
		h.t.Fatalf("bad create output %+v", out)
	}
	return out.SessionID
}

func TestTools(t *testing.T) {
	h := start(t, func(ctx context.Context, req backend.ExecRequest) (int, error) {
		switch req.Cmd[0] {
		case "flood":
			chunk := bytes.Repeat([]byte("x"), 1000)
			for range 100 {
				_, _ = req.Stdout.Write(chunk)
			}
			_, _ = io.WriteString(req.Stderr, "small")
			return 0, nil
		case "cat":
			_, err := io.Copy(req.Stdout, req.Stdin)
			return 0, err
		case "sleep":
			<-ctx.Done()
			return -1, ctx.Err()
		case "fail":
			_, _ = io.WriteString(req.Stderr, "nope\n")
			return 3, nil
		}
		_, _ = io.WriteString(req.Stdout, strings.Join(req.Cmd[1:], " ")+"\n")
		return 0, nil
	})

	tools, err := h.client.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tl := range tools.Tools {
		names = append(names, tl.Name)
	}
	slices.Sort(names)
	if want := []string{"sandbox_create", "sandbox_destroy", "sandbox_exec", "sandbox_list"}; !slices.Equal(names, want) {
		t.Fatalf("tools = %v", names)
	}

	id := h.create()
	exec := func(args map[string]any) (ExecOutput, string) {
		args["session_id"] = id
		var out ExecOutput
		e := h.call("sandbox_exec", args, &out)
		return out, e
	}

	if out, e := exec(map[string]any{"command": []string{"echo", "hi"}}); e != "" || out.Stdout != "hi\n" || out.ExitCode != 0 || out.Truncated {
		t.Fatalf("echo: %+v %q", out, e)
	}
	if out, _ := exec(map[string]any{"command": []string{"fail"}}); out.ExitCode != 3 || out.Stderr != "nope\n" {
		t.Fatalf("fail: %+v", out)
	}
	if out, _ := exec(map[string]any{"command": []string{"cat"}, "stdin": "piped"}); out.Stdout != "piped" {
		t.Fatalf("stdin: %+v", out)
	}
	if out, _ := exec(map[string]any{"command": []string{"flood"}}); len(out.Stdout) != OutputCap || !out.Truncated || out.Stderr != "small" {
		t.Fatalf("flood: stdout %d bytes, truncated=%v", len(out.Stdout), out.Truncated)
	}
	if out, e := exec(map[string]any{"command": []string{"sleep"}, "timeout_seconds": 1}); e != "" || !out.TimedOut || out.ExitCode != -1 {
		t.Fatalf("timeout: %+v %q", out, e)
	}
	if _, e := exec(map[string]any{"command": []string{}}); e == "" {
		t.Fatal("empty command accepted")
	}

	// concurrent execs on one session (the orchestrator used to refuse these)
	var wg sync.WaitGroup
	for i := range 6 {
		wg.Go(func() {
			var out ExecOutput
			e := h.call("sandbox_exec", map[string]any{"session_id": id, "command": []string{"echo", strings.Repeat("a", i)}}, &out)
			if e != "" || out.ExitCode != 0 {
				t.Errorf("concurrent exec %d: %+v %q", i, out, e)
			}
		})
	}
	wg.Wait()

	var list ListOutput
	h.call("sandbox_list", map[string]any{}, &list)
	if len(list.Sandboxes) != 1 || list.Sandboxes[0].SessionID != id || list.Sandboxes[0].State != "ready" {
		t.Fatalf("list: %+v", list)
	}

	var r receipt.Receipt
	if e := h.call("sandbox_destroy", map[string]any{"session_id": id}, &r); e != "" {
		t.Fatal("destroy:", e)
	}
	if r.Session != id || !r.Verified || !receipt.Verify(r, h.pub) {
		t.Fatalf("receipt does not verify: %+v", r)
	}
	if h.b.Len() != 0 {
		t.Fatal("sandbox survived destroy")
	}
	if _, e := exec(map[string]any{"command": []string{"echo"}}); !strings.Contains(e, "no sandbox") {
		t.Fatalf("exec after destroy: %q", e)
	}
}

func TestLimitAndShutdown(t *testing.T) {
	h := start(t, nil)
	for range DefaultMaxSessions {
		h.create()
	}
	var out CreateOutput
	if e := h.call("sandbox_create", map[string]any{}, &out); !strings.Contains(e, "limit") {
		t.Fatalf("5th create: %q", e)
	}
	if e := h.call("sandbox_create", map[string]any{"ttl": "1s"}, &out); e == "" {
		t.Fatal("invalid ttl accepted")
	}
	if h.b.Len() != DefaultMaxSessions {
		t.Fatalf("%d sandboxes, want %d", h.b.Len(), DefaultMaxSessions)
	}

	// the client going away is stdin EOF for the stdio transport
	_ = h.client.Close()
	select {
	case <-h.done:
	case <-time.After(10 * time.Second):
		t.Fatal("server did not stop when the client left")
	}
	if n := h.b.Len(); n != 0 {
		t.Fatalf("%d sandboxes survived server exit", n)
	}
}

func TestTTLExpiry(t *testing.T) {
	h := start(t, nil)
	id := h.create()
	// pull the deadline in rather than waiting minutes
	h.srv.mu.Lock()
	h.srv.sessions[id].timer.Reset(10 * time.Millisecond)
	h.srv.mu.Unlock()
	deadline := time.Now().Add(5 * time.Second)
	for h.b.Len() != 0 {
		if time.Now().After(deadline) {
			t.Fatal("sandbox outlived its ttl")
		}
		time.Sleep(10 * time.Millisecond)
	}
	var list ListOutput
	h.call("sandbox_list", map[string]any{}, &list)
	if len(list.Sandboxes) != 0 {
		t.Fatalf("expired sandbox still listed: %+v", list)
	}
}

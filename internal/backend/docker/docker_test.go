package docker

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"

	"github.com/angie-kinya/ephemeris/internal/backend"
	"github.com/angie-kinya/ephemeris/internal/spec"
)

// integration test against a real engine. skips when none is reachable,
// so `go test ./...` stays green on machines without Docker/Podman
func TestLifecycle(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	b, err := New(ctx)
	if err != nil {
		t.Skip("no container engine:", err)
	}
	defer b.Close()

	labels := map[string]string{
		backend.LabelManaged: "true", backend.LabelSession: "test" + time.Now().Format("150405"),
		backend.LabelExpires: time.Now().Add(time.Minute).Format(time.RFC3339), backend.LabelHost: "test",
	}
	sb, err := b.Create(ctx, spec.Default(), labels)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sb.Destroy(context.Background()) }) // even if asserts fail
	if err := sb.Start(ctx); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	code, err := sb.Exec(ctx, backend.ExecRequest{
		Cmd: []string{"sh", "-c", "cat; exit 4"}, Stdin: bytes.NewBufferString("ping"), Stdout: &out,
	})
	if err != nil || code != 4 || out.String() != "ping" {
		t.Fatalf("exec: code=%d out=%q err=%v", code, out.String(), err)
	}

	if err := sb.Destroy(ctx); err != nil {
		t.Fatal(err)
	}
	if err := sb.Destroy(ctx); err != nil {
		t.Fatalf("second Destroy must be a no-op, got %v", err)
	}
}

func testLabels(prefix string) map[string]string {
	return map[string]string{
		backend.LabelManaged: "true", backend.LabelSession: fmt.Sprintf("%s%x", prefix, time.Now().UnixNano()),
		backend.LabelExpires: time.Now().Add(5 * time.Minute).Format(time.RFC3339), backend.LabelHost: "test",
	}
}

func engine(t *testing.T, ctx context.Context) *Backend {
	t.Helper()
	if testing.Short() {
		t.Skip("integration test")
	}
	b, err := New(ctx)
	if err != nil {
		t.Skip("no container engine:", err)
	}
	t.Cleanup(func() { b.Close() })
	return b
}

// buildProxyBinary builds the real CLI: the test binary has no __proxy command
func buildProxyBinary(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "ephemeris")
	cmd := exec.Command("go", "build", "-o", bin, "github.com/angie-kinya/ephemeris/cmd/ephemeris")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build ephemeris: %v\n%s", err, out)
	}
	return bin
}

func TestAllowlistEgress(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	b := engine(t, ctx)
	if os.Getenv(EnvProxyImage) == "" {
		b.ProxyBinary = buildProxyBinary(t)
	}

	s := spec.Default()
	s.Image = "docker.io/curlimages/curl:8.11.1"
	s.Network = spec.Network{Mode: "allowlist", Allow: []string{"example.com"}}
	labels := testLabels("allow")
	session := labels[backend.LabelSession]

	sb, err := b.Create(ctx, s, labels)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sb.Destroy(context.Background()) })
	if err := sb.Start(ctx); err != nil {
		t.Fatal(err)
	}

	run := func(cmd ...string) (int, string) {
		var out bytes.Buffer
		code, err := sb.Exec(ctx, backend.ExecRequest{Cmd: cmd, Stdout: &out, Stderr: &out})
		if err != nil {
			t.Fatalf("exec %v: %v", cmd, err)
		}
		return code, out.String()
	}

	if code, out := run("curl", "-sS", "--max-time", "30", "-o", "/dev/null", "-w", "%{http_code}", "https://example.com/"); code != 0 || out != "200" {
		t.Fatalf("allowed host via proxy: code=%d out=%q", code, out)
	}
	if code, out := run("curl", "-sS", "--max-time", "20", "-o", "/dev/null", "https://github.com/"); code == 0 || !strings.Contains(out, "403") {
		t.Fatalf("non-allowed host must be refused by the proxy with 403: code=%d out=%q", code, out)
	}
	// bypass attempts: ignore the proxy entirely, by name and by raw IP
	if code, out := run("curl", "-sS", "--noproxy", "*", "--max-time", "10", "https://example.com/"); code == 0 {
		t.Fatalf("direct connection bypassing the proxy must fail: %q", out)
	}
	if code, out := run("curl", "-sSk", "--noproxy", "*", "--max-time", "10", "https://1.1.1.1/"); code == 0 {
		t.Fatalf("direct connection to an IP must fail: %q", out)
	}

	// List folds the proxy and network into one entry for the session
	items, err := b.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, it := range items {
		if it.Labels[backend.LabelSession] == session {
			n++
			if it.ID != sb.ID() {
				t.Errorf("session represented by %s, want the sandbox %s", short(it.ID), short(sb.ID()))
			}
		}
	}
	if n != 1 {
		t.Fatalf("List returned %d entries for the session, want 1", n)
	}

	if err := b.Remove(ctx, sb.ID()); err != nil {
		t.Fatal(err)
	}
	assertSessionGone(t, ctx, b, session)
	if err := sb.Destroy(ctx); err != nil {
		t.Fatalf("Destroy after Remove must be a no-op, got %v", err)
	}
}

// a crash between creating the network and the containers leaves only a
// labelled network. List must surface it and Remove must delete it
func TestOrphanedNetworkIsListedAndRemoved(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	b := engine(t, ctx)
	labels := testLabels("orphan")
	session := labels[backend.LabelSession]
	resp, err := b.cli.NetworkCreate(ctx, networkName(session), network.CreateOptions{Internal: true, Labels: labels})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = b.cli.NetworkRemove(context.Background(), resp.ID) })

	items, err := b.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var id string
	for _, it := range items {
		if it.Labels[backend.LabelSession] == session {
			id = it.ID
		}
	}
	if id == "" {
		t.Fatal("orphaned network not listed")
	}
	if err := b.Remove(ctx, id); err != nil {
		t.Fatal(err)
	}
	assertSessionGone(t, ctx, b, session)
	if err := b.Remove(ctx, id); err != nil {
		t.Fatalf("second Remove must be a no-op, got %v", err)
	}
}

func assertSessionGone(t *testing.T, ctx context.Context, b *Backend, session string) {
	t.Helper()
	cs, err := b.cli.ContainerList(ctx, container.ListOptions{All: true, Filters: sessionFilter(session)})
	if err != nil || len(cs) != 0 {
		t.Fatalf("containers left for session %s: %d (err %v)", session, len(cs), err)
	}
	ns, err := b.cli.NetworkList(ctx, network.ListOptions{Filters: sessionFilter(session)})
	if err != nil || len(ns) != 0 {
		t.Fatalf("networks left for session %s: %d (err %v)", session, len(ns), err)
	}
}

// a proxy that cannot start must take the network (and itself) with it
func TestAllowlistFailsClosed(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	b := engine(t, ctx)
	t.Setenv(EnvProxyImage, proxyBaseImage) // has no /ephemeris: start fails
	s := spec.Default()
	s.Network = spec.Network{Mode: "allowlist", Allow: []string{"example.com"}}
	labels := testLabels("failclosed")
	sb, err := b.Create(ctx, s, labels)
	if err == nil {
		_ = sb.Destroy(context.Background())
		t.Fatal("Create must fail when the egress proxy cannot start")
	}
	t.Log("expected error:", err)
	assertSessionGone(t, ctx, b, labels[backend.LabelSession])
}

// Package process is a sandbox built from Linux namespaces via bubblewrap
// No images, no daemon: it reuses the host's /usr read-only. its real job in
// ephemeris is to make teh user own the PTY (creack/pty), which Docker hides
package process

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/creack/pty"

	"github.com/angie-kinya/ephemeris/internal/backend"
	"github.com/angie-kinya/ephemeris/internal/spec"
)

type Backend struct{ root string }

func New() (*Backend, error) {
	// the package compiles everywhere (so the CLI does), but bubblewrap is
	// Linux namespaces: there is nothing to fall back to on macOS
	if runtime.GOOS != "linux" {
		return nil, fmt.Errorf("the process backend needs Linux namespaces (bubblewrap) and is not available on %s; use backend: docker", runtime.GOOS)
	}
	if _, err := exec.LookPath("bwrap"); err != nil {
		return nil, errors.New("process backend needs bubblewrap: dnf/apt install bubblewrap")
	}
	// $XDG_RUNTIME_DIR is a per-user tmpfs on systemd hosts: RAM, mode 0700, wiped at logout
	base := os.Getenv("XDG_RUNTIME_DIR")
	if base == "" {
		base = os.TempDir()
	}
	root := filepath.Join(base, "ephemeris")
	return &Backend{root: root}, os.MkdirAll(root, 0o700)
}

func (b *Backend) Name() string { return "process" }

type meta struct {
	Labels  map[string]string `json:"labels"`
	Created time.Time         `json:"created"`
}

func (b *Backend) Create(_ context.Context, s spec.Spec, labels map[string]string) (backend.Sandbox, error) {
	// bwrap --unshare-all leaves no network at all, and there is no sidecar
	// to put a proxy in. fail rather than silently run with less (or more) access
	if s.Network.Mode == "allowlist" {
		return nil, errors.New("network.mode allowlist is not supported by the process backend: use backend: docker")
	}
	id := labels[backend.LabelSession]
	dir, err := b.dir(id)
	if err != nil {
		return nil, err
	}
	for _, d := range []string{"workspace", "secrets"} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o700); err != nil {
			return nil, err
		}
	}
	m, _ := json.Marshal(meta{Labels: labels, Created: time.Now()})
	if err := os.WriteFile(filepath.Join(dir, "meta.json"), m, 0o600); err != nil {
		return nil, err
	}
	return &sandbox{b: b, id: id, dir: dir, spec: s}, nil
}

// dir refuses anything but hex ids: an id like "../../.ssh" would make
// remove an rm -rf of your home directory
func (b *Backend) dir(id string) (string, error) {
	if _, err := hex.DecodeString(id); err != nil || id == "" {
		return "", fmt.Errorf("invalid sandbox id %q", id)
	}
	return filepath.Join(b.root, id), nil
}

func (b *Backend) List(context.Context) ([]backend.Info, error) {
	ents, err := os.ReadDir(b.root)
	if err != nil {
		return nil, err
	}
	var out []backend.Info
	for _, e := range ents {
		raw, err := os.ReadFile(filepath.Join(b.root, e.Name(), "meta.json"))
		var m meta
		if err != nil || json.Unmarshal(raw, &m) != nil {
			continue
		}
		out = append(out, backend.Info{ID: e.Name(), Labels: m.Labels, Created: m.Created})
	}
	return out, nil
}

func (b *Backend) Remove(_ context.Context, id string) error {
	dir, err := b.dir(id)
	if err != nil {
		return err
	}
	if err := os.RemoveAll(dir); err != nil {
		// tools like the Go module cache create read-only dirs; RemoveAll
		// cannot delete entries inside them. make them writable and retry
		_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, _ error) error {
			if d != nil && d.IsDir() {
				_ = os.Chmod(p, 0o700)
			}
			return nil
		})
		if err := os.RemoveAll(dir); err != nil {
			return err
		}
	}
	if _, err := os.Stat(dir); !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("sandbox dir %s still present", dir)
	}
	return nil
}

type sandbox struct {
	b    *Backend
	id   string
	dir  string
	spec spec.Spec
}

func (s *sandbox) ID() string                        { return s.id }
func (s *sandbox) Start(context.Context) error       { return nil }
func (s *sandbox) Destroy(ctx context.Context) error { return s.b.Remove(ctx, s.id) }

func (s *sandbox) bwrapArgs(req backend.ExecRequest) []string {
	wd := req.WorkDir
	if wd == "" {
		wd = "/workspace"
	}
	a := []string{
		"--unshare-all", // new user, pid, net, ipc, uts, cgroup namespaces: no network
		"--die-with-parent",
		"--ro-bind", "/usr", "/usr",
		"--symlink", "usr/bin", "/bin", "--symlink", "usr/sbin", "/sbin",
		"--symlink", "usr/lib", "/lib", "--symlink", "usr/lib64", "/lib64",
		"--ro-bind-try", "/etc/alternatives", "/etc/alternatives",
		"--ro-bind-try", "/etc/ssl", "/etc/ssl", "--ro-bind-try", "/etc/pki", "/etc/pki",
		"--proc", "/proc", "--dev", "/dev", "--tmpfs", "/tmp",
		"--bind", filepath.Join(s.dir, "workspace"), "/workspace",
		"--bind", filepath.Join(s.dir, "secrets"), "/run/secrets",
		"--chdir", wd, "--hostname", "ephemeris", "--clearenv",
		"--setenv", "HOME", "/workspace", "--setenv", "PATH", "/usr/local/bin:/usr/bin:/bin",
		"--setenv", "EPHEMERIS", "1",
	}
	for k, v := range s.spec.Env {
		a = append(a, "--setenv", k, v)
	}
	for _, kv := range req.Env {
		if k, v, ok := strings.Cut(kv, "="); ok {
			a = append(a, "--setenv", k, v)
		}
	}
	if !req.TTY {
		// setsid stops sandboxed code from injecting keystrokes into your
		// terminal (TIOCSTI). with a TTY the shell needs the pty as its
		// controlling terminal, so pty.Start does the setsid instead
		a = append(a, "--new-session")
	}
	return append(a, "--")
}

func (s *sandbox) Exec(ctx context.Context, req backend.ExecRequest) (int, error) {
	cmd := exec.CommandContext(ctx, "bwrap", append(s.bwrapArgs(req), req.Cmd...)...)
	if !req.TTY {
		cmd.Stdin, cmd.Stdout, cmd.Stderr = req.Stdin, backend.OrDiscard(req.Stdout), backend.OrDiscard(req.Stderr)
		return exitCode(cmd.Run())
	}

	// pty.Start: opens a master/slave pair, makes the slave the child's
	// stdin/stdout/stderr and controlling terminal, returns the master to us
	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: req.InitialSize.Rows, Cols: req.InitialSize.Cols})
	if err != nil {
		return -1, fmt.Errorf("start pty: %w", err)
	}
	defer ptmx.Close()
	if req.Resize != nil {
		go func() {
			for sz := range req.Resize {
				_ = pty.Setsize(ptmx, &pty.Winsize{Rows: sz.Rows, Cols: sz.Cols}) // kernel sends SIGWINCH to the child
			}
		}()
	}
	if req.Stdin != nil {
		go func() { _, _ = io.Copy(ptmx, req.Stdin) }()
	}
	// when the child exits, reads on the master fail with EIO. on Linux that
	// is the normal end-of-session signal, not an error worth reporting
	_, _ = io.Copy(backend.OrDiscard(req.Stdout), ptmx)
	return exitCode(cmd.Wait())
}

func exitCode(err error) (int, error) {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode(), nil
	}
	if err != nil {
		return -1, err
	}
	return 0, nil
}

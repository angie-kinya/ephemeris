package main

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/angie-kinya/ephemeris/internal/backend"
	"github.com/angie-kinya/ephemeris/internal/backend/docker"
	"github.com/angie-kinya/ephemeris/internal/orchestrator"
	"github.com/angie-kinya/ephemeris/internal/reaper"
	"github.com/angie-kinya/ephemeris/internal/receipt"
	"github.com/angie-kinya/ephemeris/internal/tty"
)

func newRootCmd() *cobra.Command {
	g := &globals{}
	root := &cobra.Command{
		Use:           "ephemeris",
		Short:         "Disposable, locked-down sandboxes for code you do not fully trust",
		Version:       version,
		SilenceUsage:  true, // don't print usage for runtime errors
		SilenceErrors: true, // main() prints errors once, its own way
	}
	root.PersistentFlags().StringVarP(&g.specPath, "file", "f", "", "spec file (default ./ephemeris.yaml is present)")
	root.PersistentFlags().BoolVarP(&g.verbose, "verbose", "v", false, "debug logging")
	root.AddCommand(runCmd(g), shellCmd(g), reapCmd(g), doctorCmd(), verifyCmd())
	root.AddCommand(lsCmd(g), mcpCmd(g))
	root.AddCommand(proxyCmd())
	return root
}

func runCmd(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "run -- CMD [ARGS...]",
		Short: "Run one command in a fresh sandbox, then destroy it",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := g.setup(cmd.Context())
			if err != nil {
				return err
			}
			code, err := a.withSession(cmd.Context(), func(ctx context.Context, s *orchestrator.Session) (int, error) {
				req := backend.ExecRequest{Cmd: args, WorkDir: "/workspace", Stdout: os.Stdout, Stderr: os.Stderr}
				if !tty.IsTerminal(os.Stdin) {
					req.Stdin = os.Stdin // allow: cat data.csv | ephemeris run -- python analyse.py
				}
				return s.Exec(ctx, req)
			})
			if err != nil {
				return err
			}
			if code != 0 {
				return exitCode(code)
			}
			return nil
		},
	}
}

func shellCmd(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "shell",
		Short: "Open an interactive shell in a fresh sandbox; exit destroys it",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if !tty.IsTerminal(os.Stdin) || !tty.IsTerminal(os.Stdout) {
				return errors.New("shell needs an interactive terminal; use `ephemeris run` in scripts")
			}
			a, err := g.setup(cmd.Context())
			if err != nil {
				return err
			}
			code, err := a.withSession(cmd.Context(), func(ctx context.Context, s *orchestrator.Session) (int, error) {
				// WithRaw restores the terminal even if anything below panics,
				// and before the session's teardown logs print
				var code int
				err := tty.WithRaw(os.Stdin, func() (err error) {
					rctx, cancel := context.WithCancel(ctx)
					defer cancel() // stops WatchResize, which closes the Resize channel
					code, err = s.Exec(ctx, backend.ExecRequest{
						Cmd: a.spec.Shell, TTY: true, WorkDir: "/workspace",
						Env:         []string{"TERM=" + os.Getenv("TERM")},
						InitialSize: tty.Size(os.Stdin),
						Resize:      tty.WatchResize(rctx, os.Stdin),
						Stdin:       os.Stdin, Stdout: os.Stdout,
					})
					return err
				})
				if err != nil && code == 0 {
					code = -1
				}
				return code, err
			})
			if err != nil {
				return err
			}
			if code != 0 {
				return exitCode(code)
			}
			return nil
		},
	}
}

func reapCmd(g *globals) *cobra.Command {
	var all bool
	c := &cobra.Command{
		Use:   "reap",
		Short: "Destroy expired or orphaned sandboxes (--all: every Ephemeris sandbox)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			a, err := g.setup(cmd.Context())
			if err != nil {
				return err
			}
			res, err := reaper.Reap(cmd.Context(), a.backend, a.host, all, a.log)
			fmt.Printf("removed %d, kept %d\n", len(res.Removed), res.Kept)
			return err
		},
	}
	c.Flags().BoolVar(&all, "all", false, "remove every Ephemeris sandbox, live or not")
	return c
}

func verifyCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "verify RECEIPT.json",
		Short: "Check a teardown receipt's signature with this machine's key",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			raw, err := os.ReadFile(args[0])
			if err != nil {
				return err
			}
			var r receipt.Receipt
			if err := json.Unmarshal(raw, &r); err != nil {
				return err
			}
			key, err := receipt.LoadOrCreateKey(keyPath())
			if err != nil {
				return err
			}
			if !receipt.Verify(r, key.Public().(ed25519.PublicKey)) {
				return errors.New("INVALID: receipt was altered or signed by another key")
			}
			fmt.Printf("valid: session %s destroyed %s (verified=%v)\n", r.Session, r.Destroyed.Local().Format(time.RFC1123), r.Verified)
			return nil
		},
	}
}

func doctorCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "Check this machine's isolation options",
		RunE: func(cmd *cobra.Command, _ []string) error {
			check := func(ok bool, what, hint string) {
				mark := "ok  "
				if !ok {
					mark = "WARN"
				}
				fmt.Printf("[%s] %s", mark, what)
				if !ok && hint != "" {
					fmt.Printf("  -> %s", hint)
				}
				fmt.Println()
			}
			b, err := docker.New(cmd.Context())
			check(err == nil, "container engine reachable", "systemctl --user enable --now podman.socket; export DOCKER_HOST=unix://$XDG_RUNTIME_DIR/podman/podman.sock")
			if err == nil {
				defer b.Close()
				v, rootless, runtimes, err := b.EngineInfo(cmd.Context())
				if err == nil {
					check(true, "engine version "+v, "")
					check(rootless, "rootless engine", "a rootful daemon socket is root on this machine")
					// Podman lists runtimes from its config even when the binary is absent
					_, binErr := exec.LookPath("runsc")
					hasGvisor := strings.Contains(strings.Join(runtimes, ","), "runsc") && binErr == nil
					check(hasGvisor, "gVisor (runsc) runtime", "optional: stronger isolation, see gvisor.dev")
				}
			}
			_, err = exec.LookPath("bwrap")
			check(err == nil, "bubblewrap for the process backend", "dnf/apt install bubblewrap")
			check(exists("/dev/kvm"), "/dev/kvm for future microVM backend", "needs hardware virtualisation")
			swaps, _ := os.ReadFile("/proc/swaps")
			check(strings.Count(string(swaps), "\n") <= 1, "no swap (tmpfs secrets cannot reach disk)", "zram swap is RAM and fine; disk swap can persist secrets")
			return nil
		},
	}
}

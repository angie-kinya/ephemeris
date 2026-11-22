package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/angie-kinya/ephemeris/internal/netpolicy"
	"github.com/angie-kinya/ephemeris/internal/spec"
)

// proxyCmd is the egress proxy for network.mode allowlist. it is not meant
// for humans: the docker backend runs it inside a hardened sidecar container
// that is the sandbox's only route off its internal network
func proxyCmd() *cobra.Command {
	var (
		listen string
		allow  []string
	)
	c := &cobra.Command{
		Use:    "__proxy",
		Short:  "Run the allowlist egress proxy (internal; used by network.mode allowlist)",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return serveProxy(cmd.Context(), listen, allow)
		},
	}
	c.Flags().StringVar(&listen, "listen", ":3128", "address to listen on")
	c.Flags().StringSliceVar(&allow, "allow", nil, "allowed hosts, comma separated (example.com, *.example.com)")
	return c
}

func serveProxy(ctx context.Context, listen string, allow []string) error {
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil)).With("component", "egress")
	if len(allow) == 0 {
		return errors.New("--allow is required: an empty allowlist would deny everything")
	}
	// the spec was validated on the host already; re-check, since this
	// process is the enforcement point and must not trust its argv blindly
	for _, a := range allow {
		if err := spec.ValidAllowEntry(a); err != nil {
			return fmt.Errorf("--allow: %w", err)
		}
	}

	policy := netpolicy.New(allow, log)
	policy.Resolve = netpolicy.ResolvConfResolver("/etc/resolv.conf")
	srv := &http.Server{
		Handler: policy,
		// a slow client cannot hold a goroutine forever before it says CONNECT
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16 << 10,
		// no ReadTimeout/WriteTimeout: the deadlines they set stay on the conn
		// after Hijack and would cut long downloads (git clone, npm ci) mid-tunnel
		ErrorLog: slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}
	ln, err := net.Listen("tcp", listen)
	if err != nil {
		return err
	}
	log.Info(netpolicy.ReadyMsg, "addr", ln.Addr().String(), "allow", allow)

	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done(): // SIGTERM from `docker stop` / container removal
	}
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	log.Info("proxy shutting down")
	// Shutdown does not wait for hijacked tunnels; they die with the process
	if err := srv.Shutdown(sctx); err != nil && !errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return nil
}

// Package netpolicy decides where a sandbox may connect, and enforces it
// as an HTTPS CONNECT proxy
package netpolicy

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"slices"
	"strings"
	"syscall"
	"time"
)

// ReadyMsg is logged by `ephemeris __proxy` once it accepts connections
// the docker backend waits for it before the sandbox is allowed to start
const ReadyMsg = "proxy listening"

// ProxyPort is where the egress proxy listens inside its container
const ProxyPort = "3128"

type Policy struct {
	allow []string
	Log   *slog.Logger
	// Resolve looks up the CONNECT target. nil means the system resolver
	Resolve func(ctx context.Context, host string) ([]netip.Addr, error)
}

func New(allow []string, log *slog.Logger) Policy {
	p := Policy{Log: log}
	for _, a := range allow {
		p.allow = append(p.allow, strings.ToLower(strings.TrimSuffix(a, ".")))
	}
	return p
}

// AllowHost: exact match, or "*.example.com" matching subdomains only
// ("example.com" itself must be listed separately; "evilexample.com" never matches)
func (p Policy) AllowHost(host string) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	for _, pat := range p.allow {
		if suffix, ok := strings.CutPrefix(pat, "*"); ok {
			if strings.HasSuffix(host, suffix) && len(host) > len(suffix) {
				return true
			}
		} else if host == pat {
			return true
		}
	}
	return false
}

var cgnat = netip.MustParsePrefix("100.64.0.0/10")

// AllowIP blocks everything that is not the public internet: your LAN,
// loopback, the cloud metadata service (169.254.169.254 is link-local), etc
func AllowIP(a netip.Addr) bool {
	a = a.Unmap() // ::ffff:10.0.0.1 is 10.0.0.1 in disguise
	return a.IsValid() && a.IsGlobalUnicast() && !a.IsPrivate() && !cgnat.Contains(a)
}

// Dialer judges the address actually being dialled, after DNS. checking the
// hostname alone loses to DNS rebinding (allowed.com -> 127.0.0.1)
func (p Policy) Dialer() *net.Dialer {
	return &net.Dialer{
		Timeout: 10 * time.Second,
		Control: func(_, address string, _ syscall.RawConn) error {
			ap, err := netip.ParseAddrPort(address)
			if err != nil {
				return err
			}
			if !AllowIP(ap.Addr()) {
				return fmt.Errorf("egress to %s denied: not a public address", ap.Addr())
			}
			return nil
		},
	}
}

// dial resolves host and tries each address; Dialer still vets every one
func (p Policy) dial(ctx context.Context, host, port string) (net.Conn, error) {
	resolve := p.Resolve
	if resolve == nil {
		resolve = func(ctx context.Context, h string) ([]netip.Addr, error) {
			return net.DefaultResolver.LookupNetIP(ctx, "ip", h)
		}
	}
	addrs, err := resolve(ctx, host)
	if err != nil {
		return nil, err
	}
	// IPv4 first: container networks often have no IPv6 route at all
	slices.SortStableFunc(addrs, func(a, b netip.Addr) int {
		return cmp.Compare(boolInt(!a.Unmap().Is4()), boolInt(!b.Unmap().Is4()))
	})
	d := p.Dialer()
	var errs []error
	for _, a := range addrs {
		c, err := d.DialContext(ctx, "tcp", net.JoinHostPort(a.Unmap().String(), port))
		if err == nil {
			return c, nil
		}
		errs = append(errs, err)
	}
	if len(errs) == 0 {
		return nil, fmt.Errorf("%s: no addresses", host)
	}
	return nil, errors.Join(errs...)
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// ResolvConfResolver asks every nameserver in a resolv.conf in turn and takes
// the first answer, where the system resolver treats the first NXDOMAIN as
// final. the egress proxy needs this: it sits on an internal network too, and
// Podman lists that network's DNS server (which, correctly, refuses to resolve
// outside names for the sandbox) ahead of the real upstream resolvers
func ResolvConfResolver(path string) func(ctx context.Context, host string) ([]netip.Addr, error) {
	return func(ctx context.Context, host string) ([]netip.Addr, error) {
		if a, err := netip.ParseAddr(host); err == nil {
			return []netip.Addr{a}, nil
		}
		servers, err := nameservers(path)
		if err != nil || len(servers) == 0 {
			return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
		}
		var errs []error
		for _, ns := range servers {
			r := &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, network, net.JoinHostPort(ns, "53"))
			}}
			qctx, cancel := context.WithTimeout(ctx, 4*time.Second)
			addrs, err := r.LookupNetIP(qctx, "ip", host)
			cancel()
			if err == nil && len(addrs) > 0 {
				return addrs, nil
			}
			errs = append(errs, fmt.Errorf("nameserver %s: %w", ns, err))
			if ctx.Err() != nil {
				break
			}
		}
		return nil, errors.Join(errs...)
	}
}

func nameservers(path string) ([]string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, line := range strings.Split(string(raw), "\n") {
		f := strings.Fields(line)
		if len(f) >= 2 && f[0] == "nameserver" {
			if a, err := netip.ParseAddr(f[1]); err == nil {
				out = append(out, a.String())
			}
		}
	}
	return out, nil
}

// ServeHTTP is a CONNECT-only proxy: HTTPS clients (curl, go, npm, pip) honour
// HTTPS_PROXY and ask us to open a tunnel. we never see the TLS plaintext
func (p Policy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodConnect {
		http.Error(w, "ephemeris egress: only HTTPS via CONNECT is allowed", http.StatusMethodNotAllowed)
		return
	}
	host, port, err := net.SplitHostPort(r.Host)
	if err != nil || port != "443" || !p.AllowHost(host) {
		p.Log.Warn("egress denied", "target", r.Host)
		http.Error(w, "ephemeris egress: denied by policy", http.StatusForbidden)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	upstream, err := p.dial(ctx, host, port)
	if err != nil {
		p.Log.Warn("egress failed", "target", r.Host, "err", err)
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer upstream.Close()

	client, brw, err := http.NewResponseController(w).Hijack()
	if err != nil {
		return
	}
	defer client.Close()
	p.Log.Info("egress allowed", "target", r.Host)
	_, _ = client.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))

	// brw may already hold bytes the client sent after the CONNECT line
	go func() {
		_, _ = io.Copy(upstream, brw)
		if tc, ok := upstream.(*net.TCPConn); ok {
			_ = tc.CloseWrite()
		}
	}()
	_, _ = io.Copy(client, upstream)
}

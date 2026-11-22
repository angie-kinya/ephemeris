package netpolicy

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAllowIP(t *testing.T) {
	for addr, want := range map[string]bool{
		"1.1.1.1": true, "140.82.112.3": true,
		"127.0.0.1": false, "10.0.0.5": false, "192.168.1.1": false,
		"169.254.169.254": false, "100.64.0.1": false, "::1": false,
		"::ffff:10.0.0.1": false, "fd00::1": false, "0.0.0.0": false,
	} {
		if got := AllowIP(netip.MustParseAddr(addr)); got != want {
			t.Errorf("AllowIP(%s) = %v, want %v", addr, got, want)
		}
	}
}

func FuzzAllowHost(f *testing.F) {
	p := New([]string{"github.com", "*.npmjs.org"}, slog.Default())
	for _, s := range []string{"github.com", "registry.npmjs.org", "npmjs.org", "evilgithub.com", "GITHUB.COM."} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, host string) {
		if !p.AllowHost(host) {
			return
		}
		h := strings.ToLower(strings.TrimSuffix(host, "."))
		if h != "github.com" && !strings.HasSuffix(h, ".npmjs.org") {
			t.Fatalf("allowed %q, which matches no rule", host)
		}
	})
}

func TestServeHTTPRefusals(t *testing.T) {
	p := New([]string{"example.com"}, slog.New(slog.DiscardHandler))
	// DNS rebinding: an allowed name that resolves to loopback must not be dialled
	p.Resolve = func(context.Context, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
	}
	for _, c := range []struct {
		method, target string
		want           int
	}{
		{http.MethodGet, "example.com:80", http.StatusMethodNotAllowed},
		{http.MethodConnect, "github.com:443", http.StatusForbidden},
		{http.MethodConnect, "example.com:22", http.StatusForbidden},
		{http.MethodConnect, "example.com:443", http.StatusBadGateway},
	} {
		r := httptest.NewRequest(c.method, "http://"+c.target, nil)
		r.Host = c.target
		w := httptest.NewRecorder()
		p.ServeHTTP(w, r)
		if w.Code != c.want {
			t.Errorf("%s %s = %d, want %d (%s)", c.method, c.target, w.Code, c.want, w.Body)
		}
	}
}

func TestNameservers(t *testing.T) {
	path := filepath.Join(t.TempDir(), "resolv.conf")
	os.WriteFile(path, []byte("search dns.podman\nnameserver 10.89.0.1\n# nameserver 9.9.9.9\nnameserver 169.254.1.1\nnameserver bogus\noptions ndots:1\n"), 0o600)
	got, err := nameservers(path)
	if err != nil || strings.Join(got, ",") != "10.89.0.1,169.254.1.1" {
		t.Fatalf("got %v, %v", got, err)
	}
}

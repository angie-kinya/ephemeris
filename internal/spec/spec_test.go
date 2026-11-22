package spec

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLoadMergesOverDefaults(t *testing.T) {
	p := filepath.Join(t.TempDir(), "ephemeris.yaml")
	os.WriteFile(p, []byte("name: demo\nttl: 45m\nresources: {cpu: 2}\n"), 0o600)

	s, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if s.TTL != 45*time.Minute || s.Resources.CPU != 2 {
		t.Fatalf("overrides not applied: %+v", s)
	}
	if s.Resources.Memory != 1<<30 {
		t.Fatalf("default memory lost when only cpu was set: %d", s.Resources.Memory)
	}
}

func TestValidateReportsAllErrors(t *testing.T) {
	s := Default()
	s.Name = "Bad Name"
	s.Secrets = []Secret{{Name: "lower", From: "vault:x"}}
	err := s.Validate()
	if err == nil {
		t.Fatal("want error")
	}
	for _, want := range []string{"name", "secrets[0].name", "secrets[0].from"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("missing %q in: %v", want, err)
		}
	}
}

func TestUnknownFieldRejected(t *testing.T) {
	p := filepath.Join(t.TempDir(), "e.yaml")
	os.WriteFile(p, []byte("secrests: []\n"), 0o600)
	if _, err := Load(p); err == nil {
		t.Fatal("typo in field name must fail")
	}
}

func FuzzParseBytes(f *testing.F) {
	for _, s := range []string{"1GiB", "512MiB", "0.5GiB", "10B", "", "-1KiB", "NaNGiB"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		if v, err := ParseBytes(s); err == nil && v < 0 {
			t.Fatalf("%q parsed to negative %d", s, v)
		}
	})
}

func TestNetworkAllowlist(t *testing.T) {
	mk := func(mode string, allow ...string) Spec {
		s := Default()
		s.Network = Network{Mode: mode, Allow: allow}
		return s
	}
	cases := []struct {
		name string
		s    Spec
		want string // "" = valid; else a substring of the error
	}{
		{"exact and wildcard", mk("allowlist", "example.com", "*.npmjs.org", "Registry.NPMjs.org."), ""},
		{"empty list", mk("allowlist"), "non-empty network.allow"},
		{"ip literal", mk("allowlist", "1.1.1.1"), "IP addresses"},
		{"ipv6 literal", mk("allowlist", "::1"), "bare hostname"},
		{"scheme", mk("allowlist", "https://example.com"), "bare hostname"},
		{"port", mk("allowlist", "example.com:443"), "bare hostname"},
		{"path", mk("allowlist", "example.com/x"), "bare hostname"},
		{"comma smuggles a second entry", mk("allowlist", "a.com,b.com"), "bare hostname"},
		{"bare star", mk("allowlist", "*"), "wildcard"},
		{"inner wildcard", mk("allowlist", "a.*.com"), "wildcard"},
		{"glued wildcard", mk("allowlist", "*example.com"), "wildcard"},
		{"tld wildcard", mk("allowlist", "*.com"), "fully qualified"},
		{"single label", mk("allowlist", "localhost"), "fully qualified"},
		{"bad label", mk("allowlist", "-bad.example.com"), "DNS label"},
		{"empty label", mk("allowlist", "a..com"), "DNS label"},
		{"empty entry", mk("allowlist", ""), "empty host"},
		{"allow without allowlist", mk("open", "example.com"), "only used with"},
		{"none is still fine", mk("none"), ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := c.s.Validate()
			switch {
			case c.want == "" && err != nil:
				t.Fatalf("want valid, got %v", err)
			case c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)):
				t.Fatalf("want error containing %q, got %v", c.want, err)
			}
		})
	}

	p := mk("allowlist", "example.com")
	p.Backend = "process"
	if err := p.Validate(); err == nil || !strings.Contains(err.Error(), "docker backend") {
		t.Fatalf("process backend must reject allowlist, got %v", err)
	}
}

func TestLoadAllowlistYAML(t *testing.T) {
	p := filepath.Join(t.TempDir(), "e.yaml")
	os.WriteFile(p, []byte("network:\n  mode: allowlist\n  allow: [example.com, \"*.github.com\"]\n"), 0o600)
	s, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if s.Network.Mode != "allowlist" || len(s.Network.Allow) != 2 {
		t.Fatalf("got %+v", s.Network)
	}
}

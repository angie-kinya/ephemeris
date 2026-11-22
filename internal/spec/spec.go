package spec

import (
	"errors"
	"fmt"
	"io"
	"net/netip"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type Spec struct {
	Name             string            `yaml:"name"`
	Backend          string            `yaml:"backend"` // docker | process
	Image            string            `yaml:"image"`   // fully qualified: podman refuses short names
	Runtime          string            `yaml:"runtime"` // "" = engine default, "runsc" = gVisor
	Shell            []string          `yaml:"shell"`
	Env              map[string]string `yaml:"env"`
	TTL              time.Duration     `yaml:"ttl"`
	ProvisionTimeout time.Duration     `yaml:"provision_timeout"`
	Resources        Resources         `yaml:"resources"`
	Source           Source            `yaml:"source"`
	Secrets          []Secret          `yaml:"secrets"`
	Network          Network           `yaml:"network"`
}

type Resources struct {
	CPU    float64  `yaml:"cpu"`
	Memory ByteSize `yaml:"memory"`
	PIDs   int64    `yaml:"pids"`
	Disk   ByteSize `yaml:"disk"` // size of the tmpfs workspace (counts against RAM!)
}

type Source struct {
	Path string `yaml:"path"` // local git repo, e.g. "."
	Git  string `yaml:"git"`  // remote URL; wins over Path
	Ref  string `yaml:"ref"`  // branch, tag, sha or pull/N/head
}

type Secret struct {
	Name string `yaml:"name"` // becomes /run/secrets/<Name>
	From string `yaml:"from"` // env:VAR | file:/path
}

type Network struct {
	Mode  string   `yaml:"mode"`  // none | open | allowlist
	Allow []string `yaml:"allow"` // allowlist only: "host.example.com" or "*.example.com"
	// ports are published on 127.0.0.1 only (same number on both sides), so a
	// browser on this machine can reach a dev server; nothing on the LAN can
	Ports []int `yaml:"ports"`
}

type ByteSize int64

func (b *ByteSize) UnmarshalYAML(n *yaml.Node) error {
	v, err := ParseBytes(n.Value)
	if err != nil {
		return fmt.Errorf("line %d: %w", n.Line, err)
	}
	*b = v
	return nil
}

func ParseBytes(s string) (ByteSize, error) {
	s = strings.TrimSpace(s)
	for _, u := range []struct {
		suffix string
		mul    float64
	}{{"KiB", 1 << 10}, {"MiB", 1 << 20}, {"GiB", 1 << 30}, {"B", 1}} {
		if num, ok := strings.CutSuffix(s, u.suffix); ok {
			f, err := strconv.ParseFloat(strings.TrimSpace(num), 64)
			if err != nil || !(f > 0 && f <= 1<<20) {
				return 0, fmt.Errorf("invalid size %q", s)
			}
			return ByteSize(f * u.mul), nil
		}
	}
	return 0, fmt.Errorf("invalid size %q: use KiB, MiB, or GiB", s)
}

// what ephemeris will use when no ephemeris.yaml is present
func Default() Spec {
	return Spec{
		Name:             "default",
		Backend:          "docker",
		Image:            "docker.io/library/alpine:3.20",
		Shell:            []string{"/bin/sh"},
		TTL:              30 * time.Minute,
		ProvisionTimeout: 3 * time.Minute,
		Resources:        Resources{CPU: 1, Memory: 1 << 30, PIDs: 256, Disk: 512 << 20},
		Network:          Network{Mode: "none"},
	}
}

func Load(path string) (Spec, error) {
	s := Default()
	f, err := os.Open(path)
	if err != nil {
		return Spec{}, err
	}
	defer f.Close()
	dec := yaml.NewDecoder(f)
	dec.KnownFields(true) // typos must fail, not just do nothing
	if err := dec.Decode(&s); err != nil && !errors.Is(err, io.EOF) {
		return Spec{}, fmt.Errorf("parse %s: %w", path, err)
	}
	return s, s.Validate()
}

var (
	nameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,40}$`)
	envRe  = regexp.MustCompile(`^[A-Z_][A-Z0-9_]*$`)
)

func (s Spec) Validate() error {
	var errs []error
	bad := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }

	if !nameRe.MatchString(s.Name) {
		bad("name %q: use lowercase letters, digits and dashes", s.Name)
	}
	switch s.Backend {
	case "docker":
		if s.Image == "" {
			bad("image is required for the docker backend")
		}
	case "process":
	default:
		bad("backend %q: want docker or process", s.Backend)
	}
	if s.TTL < time.Minute || s.TTL > 24*time.Hour {
		bad("ttl %s: must be between 1m and 24h", s.TTL)
	}
	if s.ProvisionTimeout <= 0 {
		bad("provision_timeout must be positive")
	}
	if len(s.Shell) == 0 {
		bad("shell must not be empty")
	}
	r := s.Resources
	if r.CPU <= 0 || r.Memory < 64<<20 || r.PIDs < 16 || r.Disk < 16<<20 {
		bad("resources: need cpu > 0, memory >= 64MiB, pids >= 16, disk >= 16MiB")
	}
	seen := map[string]bool{}
	for i, sec := range s.Secrets {
		if !envRe.MatchString(sec.Name) {
			bad("secrets[%d].name %q: use UPPER_SNAKE_CASE", i, sec.Name)
		}
		if seen[sec.Name] {
			bad("secrets[%d].name %q is duplicated", i, sec.Name)
		}
		seen[sec.Name] = true
		if !strings.HasPrefix(sec.From, "env:") && !strings.HasPrefix(sec.From, "file:") {
			bad("secrets[%d].from %q: want env:NAME or file:/path", i, sec.From)
		}
	}
	for k := range s.Env {
		if !envRe.MatchString(k) {
			bad("env %q: use UPPER_SNAKE_CASE", k)
		}
	}
	switch s.Network.Mode {
	case "none", "open":
		if len(s.Network.Allow) > 0 {
			bad("network.allow is only used with network.mode allowlist (mode is %q)", s.Network.Mode)
		}
	case "allowlist":
		if s.Backend != "docker" {
			bad("network.mode allowlist needs the docker backend (the process backend has no egress proxy)")
		}
		if len(s.Network.Allow) == 0 {
			bad("network.mode allowlist needs a non-empty network.allow list")
		}
		for i, a := range s.Network.Allow {
			if err := ValidAllowEntry(a); err != nil {
				bad("network.allow[%d]: %v", i, err)
			}
		}
	default:
		bad("network.mode %q: want none, open or allowlist", s.Network.Mode)
	}
	if len(s.Network.Ports) > 0 && (s.Network.Mode != "open" || s.Backend != "docker") {
		// none has no interface to forward to; allowlist sits on an internal
		// network, which engines refuse to publish from
		bad("network.ports needs network.mode open and the docker backend")
	}
	seenPort := map[int]bool{}
	for i, p := range s.Network.Ports {
		if p < 1 || p > 65535 || seenPort[p] {
			bad("network.ports[%d] = %d: want a unique port 1-65535", i, p)
		}
		seenPort[p] = true
	}
	return errors.Join(errs...)
}

// one DNS label: letters, digits and inner dashes, at most 63 bytes
var labelRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// ValidAllowEntry accepts "example.com" (that host only) or "*.example.com"
// (its subdomains only). the proxy matches on the CONNECT hostname, so IP
// literals, ports, schemes, paths and partial wildcards are all mistakes
// a bare TLD ("com", "*.com") is rejected as far too broad to mean anything
func ValidAllowEntry(entry string) error {
	host := strings.ToLower(strings.TrimSuffix(entry, "."))
	if rest, ok := strings.CutPrefix(host, "*."); ok {
		host = rest
	}
	switch {
	case host == "":
		return fmt.Errorf("%q: empty host", entry)
	case strings.Contains(host, "://") || strings.ContainsAny(host, "/:@ ,"):
		return fmt.Errorf("%q: want a bare hostname like example.com (no scheme, port, path or commas)", entry)
	case strings.Contains(host, "*"):
		return fmt.Errorf("%q: a wildcard is only allowed as a leading \"*.\"", entry)
	}
	if _, err := netip.ParseAddr(host); err == nil {
		return fmt.Errorf("%q: IP addresses are not supported; list the hostname clients connect to", entry)
	}
	if len(host) > 253 || !strings.Contains(host, ".") {
		return fmt.Errorf("%q: want a fully qualified hostname like example.com", entry)
	}
	for _, l := range strings.Split(host, ".") {
		if !labelRe.MatchString(l) {
			return fmt.Errorf("%q: %q is not a valid DNS label", entry, l)
		}
	}
	return nil
}

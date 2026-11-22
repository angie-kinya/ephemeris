package reaper

import (
	"testing"
	"time"

	"github.com/angie-kinya/ephemeris/internal/backend"
)

func TestVerdict(t *testing.T) {
	now := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	future := now.Add(time.Hour).Format(time.RFC3339)
	info := func(host, exp string, age time.Duration) backend.Info {
		return backend.Info{
			Labels:  map[string]string{backend.LabelHost: host, backend.LabelExpires: exp, backend.LabelSession: "abc"},
			Created: now.Add(-age),
		}
	}
	dead := func(string) bool { return false }
	live := func(string) bool { return true }

	cases := []struct {
		name  string
		it    backend.Info
		alive func(string) bool
		kill  bool
	}{
		{"healthy", info("me", future, time.Minute), live, false},
		{"expired", info("me", now.Add(-time.Second).Format(time.RFC3339), time.Hour), live, true},
		{"crashed owner", info("me", future, time.Minute), dead, true},
		{"brand new, lease not seen yet", info("me", future, time.Second), dead, false},
		{"other host, no lease here", info("laptop-2", future, time.Minute), dead, false},
		{"garbage label", info("me", "soon", time.Minute), live, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Verdict(c.it, "me", now, c.alive) != ""
			if got != c.kill {
				t.Fatalf("kill=%v, want %v", got, c.kill)
			}
		})
	}
}

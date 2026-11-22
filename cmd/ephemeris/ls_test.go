package main

import (
	"strings"
	"testing"
	"time"

	"github.com/angie-kinya/ephemeris/internal/backend"
)

func TestPrintSandboxes(t *testing.T) {
	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	row := func(id, host, role string, age, left time.Duration) lsRow {
		l := map[string]string{
			backend.LabelSession: id, backend.LabelHost: host,
			backend.LabelExpires: now.Add(left).Format(time.RFC3339),
		}
		if role != "" {
			l[labelRole] = role
		}
		return lsRow{backend: "docker", info: backend.Info{Labels: l, Created: now.Add(-age)}}
	}
	var sb strings.Builder
	printSandboxes(&sb, []lsRow{
		row("bbb", "other", "", 2*time.Hour+5*time.Minute, -time.Minute),
		row("aaa", "me", "proxy", 30*time.Second, 29*time.Minute),
	}, "me", now)
	lines := strings.Split(strings.TrimSpace(sb.String()), "\n")
	if len(lines) != 3 {
		t.Fatalf("got:\n%s", sb.String())
	}
	for _, want := range []string{"SESSION", "ROLE", "TIME LEFT", "LEASE"} {
		if !strings.Contains(lines[0], want) {
			t.Errorf("header %q lacks %s", lines[0], want)
		}
	}
	// oldest first; other host's lease is unknowable
	if f := strings.Fields(lines[1]); strings.Join(f, " ") != "bbb docker - 2h05m expired other -" {
		t.Errorf("row 1 = %q", f)
	}
	if f := strings.Fields(lines[2]); strings.Join(f, " ") != "aaa docker proxy 30s 29m me dead" {
		t.Errorf("row 2 = %q", f)
	}
}

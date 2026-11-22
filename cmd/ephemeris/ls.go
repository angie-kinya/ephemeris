package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/angie-kinya/ephemeris/internal/backend"
	"github.com/angie-kinya/ephemeris/internal/lease"
)

// labelRole marks what a sandbox is for when it is not a plain session
// (e.g. a helper container). shown by ls only when present
const labelRole = "dev.ephemeris.role"

type lsRow struct {
	backend string
	info    backend.Info
}

func lsCmd(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:     "ls",
		Aliases: []string{"list", "ps"},
		Short:   "List Ephemeris sandboxes on every reachable backend",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			rows, err := listAll(cmd.Context(), g.verbose)
			if len(rows) == 0 && err != nil {
				return err
			}
			host, _ := os.Hostname()
			printSandboxes(cmd.OutOrStdout(), rows, host, time.Now())
			return nil
		},
	}
}

// listAll asks every backend, not just the spec's: a sandbox you forgot about
// is exactly the one started with a different spec. unreachable backends are
// skipped; the error is returned only so ls can fail when none answered
func listAll(ctx context.Context, verbose bool) ([]lsRow, error) {
	var (
		rows []lsRow
		errs []error
	)
	for _, name := range []string{"docker", "process"} {
		b, err := newBackend(ctx, name)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", name, err))
			if verbose {
				fmt.Fprintln(os.Stderr, "ls: skipping backend:", err)
			}
			continue
		}
		items, err := b.List(ctx)
		if c, ok := b.(io.Closer); ok {
			_ = c.Close()
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", name, err))
			continue
		}
		for _, it := range items {
			rows = append(rows, lsRow{backend: name, info: it})
		}
	}
	return rows, errors.Join(errs...)
}

func printSandboxes(w io.Writer, rows []lsRow, host string, now time.Time) {
	slices.SortFunc(rows, func(a, b lsRow) int { return a.info.Created.Compare(b.info.Created) })
	showRole := slices.ContainsFunc(rows, func(r lsRow) bool { return r.info.Labels[labelRole] != "" })

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	defer tw.Flush()
	header := "SESSION\tBACKEND\t"
	if showRole {
		header += "ROLE\t"
	}
	fmt.Fprintln(tw, header+"AGE\tTIME LEFT\tHOST\tLEASE")
	for _, r := range rows {
		l := r.info.Labels
		line := orDash(l[backend.LabelSession]) + "\t" + r.backend + "\t"
		if showRole {
			line += orDash(l[labelRole]) + "\t"
		}
		left := "?"
		if exp, err := time.Parse(time.RFC3339, l[backend.LabelExpires]); err == nil {
			if d := exp.Sub(now); d > 0 {
				left = human(d)
			} else {
				left = "expired"
			}
		}
		// leases are local files: for another host's sandbox we cannot know
		alive := "-"
		if l[backend.LabelHost] == host {
			alive = "dead"
			if lease.Alive(l[backend.LabelSession], now) {
				alive = "alive"
			}
		}
		fmt.Fprintln(tw, line+human(now.Sub(r.info.Created))+"\t"+left+"\t"+orDash(l[backend.LabelHost])+"\t"+alive)
	}
}

// human is a short duration: 45s, 12m, 3h05m
func human(d time.Duration) string {
	d = max(d, 0) // clock skew between us and a remote engine
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	default:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	}
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

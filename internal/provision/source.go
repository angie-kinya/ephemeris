// Package provision holds the Provisioners: each fills a started sandbox with one thing
package provision

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"github.com/angie-kinya/ephemeris/internal/backend"
	"github.com/angie-kinya/ephemeris/internal/spec"
)

// Source streams a git tree into /workspace as a tar over exec stdin
// the sandbox needs no network and no git: the host does the git work
type Source struct{}

func (Source) Name() string { return "source" }

func (Source) Provision(ctx context.Context, sb backend.Sandbox, s spec.Spec) error {
	dir := s.Source.Path
	if s.Source.Git != "" {
		tmp, err := os.MkdirTemp("", "ephemeris-src-")
		if err != nil {
			return err
		}
		defer os.RemoveAll(tmp)
		ref := s.Source.Ref
		if ref == "" {
			ref = "HEAD"
		}
		// init + fetch handles branches, tags, SHAs and pull/N/head alike.
		if err := git(ctx, tmp, "init", "-q"); err != nil {
			return err
		}
		if err := git(ctx, tmp, "fetch", "-q", "--depth=1", s.Source.Git, ref); err != nil {
			return err
		}
		dir = tmp
	}
	if dir == "" {
		return nil // nothing to ship
	}

	tree := "HEAD"
	if s.Source.Git == "" {
		// `git stash create` makes a commit of your uncommitted tracked changes
		// without touching your stash or working tree. empty output = clean tree
		out, err := exec.CommandContext(ctx, "git", "-C", dir, "stash", "create").Output()
		if err != nil {
			return fmt.Errorf("%s is not a git repository (set source: {} to ship nothing): %w", dir, err)
		}
		if sha := strings.TrimSpace(string(out)); sha != "" {
			tree = sha
		}
	} else {
		tree = "FETCH_HEAD"
	}

	// Pipe: git archive writes the tar, the sandbox's tar reads it. no temp file
	pr, pw := io.Pipe()
	defer pr.Close() // if Exec fails early, this unblocks git's writes
	archive := exec.CommandContext(ctx, "git", "-C", dir, "archive", "--format=tar", tree)
	archive.Stdout = pw
	var gitErr bytes.Buffer
	archive.Stderr = &gitErr
	if err := archive.Start(); err != nil {
		return err
	}
	waitErr := make(chan error, 1)
	go func() {
		err := archive.Wait()
		pw.CloseWithError(err) // nil -> reader sees clean EOF
		waitErr <- err
	}()

	var stderr bytes.Buffer
	// -o: don't restore file owners (CAP_CHOWN is dropped on purpose)
	code, err := sb.Exec(ctx, backend.ExecRequest{
		Cmd: []string{"tar", "-xo", "-C", "/workspace", "-f", "-"}, Stdin: pr, Stderr: &stderr,
	})
	pr.Close()
	if gerr := <-waitErr; gerr != nil {
		return fmt.Errorf("git archive: %v: %s", gerr, gitErr.String())
	}
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("untar in sandbox exited %d: %s", code, stderr.String())
	}
	return nil
}

func git(ctx context.Context, dir string, args ...string) error {
	out, err := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("git %s: %v: %s", args[0], err, out)
	}
	return nil
}

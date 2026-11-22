package provision

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/angie-kinya/ephemeris/internal/backend"
	"github.com/angie-kinya/ephemeris/internal/spec"
)

// Secrets writes each secret to /run/secrets/<NAME> (tmpfs, 0700 dir, 0600 file)
type Secrets struct{}

func (Secrets) Name() string { return "secrets" }

func (Secrets) Provision(ctx context.Context, sb backend.Sandbox, s spec.Spec) error {
	for _, sec := range s.Secrets {
		val, err := resolve(sec.From)
		if err != nil {
			return fmt.Errorf("secret %s: %w", sec.Name, err)
		}
		var stderr bytes.Buffer
		code, err := sb.Exec(ctx, backend.ExecRequest{
			Cmd:    []string{"sh", "-c", `umask 077 && cat > "/run/secrets/$1"`, "sh", sec.Name},
			Stdin:  bytes.NewReader(val),
			Stderr: &stderr,
		})
		clear(val)
		if err != nil {
			return err
		}
		if code != 0 {
			return fmt.Errorf("write secret %s: exit %d: %s", sec.Name, code, stderr.String())
		}
	}
	return nil
}

func resolve(from string) ([]byte, error) {
	kind, ref, _ := strings.Cut(from, ":")
	switch kind {
	case "env":
		v, ok := os.LookupEnv(ref)
		if !ok {
			return nil, fmt.Errorf("host env var %s is not set", ref)
		}
		return []byte(v), nil
	case "file":
		return os.ReadFile(ref)
	}
	return nil, fmt.Errorf("unknown source %q", kind)
}

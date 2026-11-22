package main

import (
	"context"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/spf13/cobra"

	"github.com/angie-kinya/ephemeris/internal/mcpserver"
	"github.com/angie-kinya/ephemeris/internal/orchestrator"
	"github.com/angie-kinya/ephemeris/internal/spec"
)

func mcpCmd(g *globals) *cobra.Command {
	var (
		maxSessions   int
		allowSpecPath bool
	)
	c := &cobra.Command{
		Use:   "mcp",
		Short: "Serve sandboxes to AI agents over MCP (stdio); all are destroyed on exit",
		Long: `Runs a Model Context Protocol server on stdin/stdout with the tools
sandbox_create, sandbox_exec, sandbox_destroy and sandbox_list.
stdout carries only the protocol; logs go to stderr.

Register it with Claude Code:
  claude mcp add ephemeris -- ephemeris mcp`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			a, err := g.setup(ctx)
			if err != nil {
				return err
			}
			defer a.reapInBackground(ctx)()

			srv := mcpserver.New(mcpserver.Config{
				Spawn: func(ctx context.Context, specPath string, ttl time.Duration) (*orchestrator.Session, error) {
					s := a.spec
					if specPath != "" {
						loaded, err := spec.Load(specPath) // local err: creates run concurrently
						if err != nil {
							return nil, err
						}
						s = loaded
					}
					if ttl > 0 {
						s.TTL = ttl
						if err := s.Validate(); err != nil {
							return nil, err
						}
					}
					return a.spawn(ctx, s)
				},
				Destroy:       a.destroy,
				Log:           a.log,
				MaxSessions:   maxSessions,
				AllowSpecPath: allowSpecPath,
				Version:       version,
			})
			a.log.Info("mcp server listening on stdio", "backend", a.spec.Backend, "max_sessions", maxSessions)
			return srv.Run(ctx, &mcp.StdioTransport{})
		},
	}
	c.Flags().BoolVar(&allowSpecPath, "allow-spec-path", false, "let clients choose a spec file (can expose host files via secrets)")
	c.Flags().IntVar(&maxSessions, "max-sessions", mcpserver.DefaultMaxSessions, "most sandboxes that may exist at once")
	return c
}

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"
)

var version = "dev"

func main() { os.Exit(run()) }

// run exists so every defer executes befor os.Exit
func run() int {
	// Ctrl-C (outside raw mode), `kill`, or closing the terminal all cancel ctx,
	// and cancellation flows down into every backend call
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()

	if err := newRootCmd().ExecuteContext(ctx); err != nil {
		var code exitCode
		if errors.As(err, &code) {
			return int(code) // the sandboxed command's own exit status
		}
		fmt.Fprintln(os.Stderr, "ephemeris:", err)
		return 1
	}
	return 0
}

// exitCode carries a non-zero status from inside the sandbox to the process,
// so `ephemeris run -- make test` works in CI and shell scripts
type exitCode int

func (e exitCode) Error() string { return fmt.Sprintf("exit status %d", int(e)) }

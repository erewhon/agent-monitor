// Command agent-monitor is the standalone binary for the agent-monitor TUI.
// All behaviour lives in github.com/erewhon/agent-monitor/cli so that other
// tools (e.g. pitf) can mount it as a subcommand.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/erewhon/agent-monitor/cli"
)

// version is set at build time via ldflags (-X main.version=...)
var version = "dev"

func main() {
	cli.Version = version

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	err := cli.Run(ctx, os.Args[1:])
	if err == nil || errors.Is(err, flag.ErrHelp) {
		return
	}
	var usageErr *cli.UsageError
	if errors.As(err, &usageErr) {
		// The FlagSet already printed the error and usage.
		os.Exit(2)
	}
	fmt.Fprintf(os.Stderr, "Error: %v\n", err)
	os.Exit(1)
}

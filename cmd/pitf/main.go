// Command pitf is one entry point over the smithy LLM tools: agent-monitor,
// tokenator, the llm-router-go commands, and the Python ops tools behind
// git-style external subcommands. See README.md.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/erewhon/pitf/internal/cli"
)

// version is stamped at build time via -ldflags "-X main.version=...".
var version = "dev"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	err := cli.Main(ctx, version, os.Args[1:])
	if err == nil {
		return
	}
	var exit *cli.ExitError
	if errors.As(err, &exit) {
		os.Exit(exit.Code)
	}
	fmt.Fprintln(os.Stderr, "pitf:", err)
	os.Exit(1)
}

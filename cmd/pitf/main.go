// Command pitf is one entry point over the smithy LLM tools: agent-monitor,
// tokenator, the llm-router-go commands, and the Python ops tools behind
// git-style external subcommands. See README.md.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/erewhon/pitf/internal/cli"
)

// version is stamped at build time via -ldflags "-X main.version=...".
var version = "dev"

func main() {
	// No signal context here on purpose: each mounted tool decides whether
	// Ctrl-C cancels a context or kills the process, matching its standalone
	// behaviour (see internal/cli/mount_*.go). Externals replace the process.
	err := cli.Main(context.Background(), version, os.Args[1:])
	if err == nil {
		return
	}
	var exit *cli.ExitError
	if errors.As(err, &exit) {
		if exit.Msg != "" {
			fmt.Fprintln(os.Stderr, exit.Msg)
		}
		os.Exit(exit.Code)
	}
	fmt.Fprintln(os.Stderr, "pitf:", err)
	os.Exit(1)
}

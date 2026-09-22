//go:build unix

package cli

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
)

// runExternal replaces the current process with the external command so
// the terminal, signals, and exit status are all its own. On success it
// never returns.
func runExternal(_ context.Context, path string, args []string) error {
	argv := append([]string{filepath.Base(path)}, args...)
	return syscall.Exec(path, argv, os.Environ())
}

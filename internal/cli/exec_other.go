//go:build !unix

package cli

import (
	"context"
	"errors"
	"os"
	"os/exec"
)

// runExternal runs the external command as a child and forwards its exit
// status, for platforms without exec(2).
func runExternal(ctx context.Context, path string, args []string) error {
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	err := cmd.Run()
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return &ExitError{Code: ee.ExitCode()}
	}
	return err
}

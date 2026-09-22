package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// buildPitf compiles the real binary once per test run so dispatch (which
// exec(2)s and never returns) can be observed from outside the process.
func buildPitf(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("exec semantics")
	}
	bin := filepath.Join(t.TempDir(), "pitf")
	cmd := exec.Command("go", "build", "-o", bin, "../../cmd/pitf")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	return bin
}

func run(t *testing.T, bin, pathEnv string, args ...string) (string, int) {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Env = append(os.Environ(), "PATH="+pathEnv)
	out, err := cmd.CombinedOutput()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("run %v: %v\n%s", args, err, out)
	}
	return string(out), code
}

func TestDispatchRunsExternalWithArgsAndExitCode(t *testing.T) {
	bin := buildPitf(t)
	dir := t.TempDir()
	writeExec(t, dir, "pitf-hello", "#!/bin/sh\necho \"hello: $*\"\nexit 7\n")
	pathEnv := dir + string(os.PathListSeparator) + os.Getenv("PATH")

	out, code := run(t, bin, pathEnv, "hello", "--name", "world", "-v")
	if !strings.Contains(out, "hello: --name world -v") {
		t.Fatalf("external did not receive its args verbatim: %q", out)
	}
	if code != 7 {
		t.Fatalf("exit code = %d, want 7 (propagated from external)", code)
	}
}

func TestBuiltinWinsOverExternal(t *testing.T) {
	bin := buildPitf(t)
	dir := t.TempDir()
	writeExec(t, dir, "pitf-completion", "#!/bin/sh\necho SHADOWED\nexit 3\n")
	pathEnv := dir + string(os.PathListSeparator) + os.Getenv("PATH")

	out, code := run(t, bin, pathEnv, "completion", "bash")
	if code != 0 || strings.Contains(out, "SHADOWED") {
		t.Fatalf("built-in completion was shadowed by an external (code %d): %q", code, out)
	}
}

func TestUnknownCommandListsExternals(t *testing.T) {
	bin := buildPitf(t)
	dir := t.TempDir()
	writeExec(t, dir, "pitf-bench", "#!/bin/sh\n")
	out, code := run(t, bin, dir, "nope")
	if code == 0 {
		t.Fatal("unknown command exited 0")
	}
	if !strings.Contains(out, `unknown command "nope"`) || !strings.Contains(out, "  bench") {
		t.Fatalf("unknown-command error should name the externals: %q", out)
	}
}

func TestHelpAndVersion(t *testing.T) {
	bin := buildPitf(t)
	dir := t.TempDir()
	writeExec(t, dir, "pitf-qual", "#!/bin/sh\n")
	out, code := run(t, bin, dir, "help")
	if code != 0 || !strings.Contains(out, "External Commands") || !strings.Contains(out, "  qual") {
		t.Fatalf("help should list externals (code %d): %q", code, out)
	}
	out, code = run(t, bin, dir, "--version")
	if code != 0 || !strings.HasPrefix(out, "pitf ") {
		t.Fatalf("--version (code %d): %q", code, out)
	}
}

package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// runPy runs the built binary with a controlled PATH, smithy dir and an
// empty config, so the Python-tool dispatch is observable from outside.
func runPy(t *testing.T, bin, pathEnv, smithy string, args ...string) (string, int) {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Env = append(os.Environ(), "PATH="+pathEnv, "PITF_SMITHY_DIR="+smithy,
		"XDG_CONFIG_HOME="+t.TempDir(), "PITF_PROFILE=", "ROUTER_API_KEY=", "PITF_ROUTER_URL=")
	out, err := cmd.CombinedOutput()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("run %v: %v\n%s", args, err, out)
	}
	return string(out), code
}

func TestPyToolExecsUVInTheProject(t *testing.T) {
	bin := buildPitf(t)
	smithy := t.TempDir()
	proj := filepath.Join(smithy, "llm-router")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(proj, "pyproject.toml"), []byte("[project]\nname='x'\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	pathDir := t.TempDir()
	writeExec(t, pathDir, "uv", "#!/bin/sh\necho \"uv: $*\"\necho \"smithy=$PITF_SMITHY_DIR\"\nexit 5\n")

	out, code := runPy(t, bin, pathDir, smithy, "qual", "--flag", "x")
	want := "uv: run -q --project " + proj + " llm-router-qual --flag x"
	if !strings.Contains(out, want) {
		t.Fatalf("qual should exec uv in the project:\n got %q\nwant %q", out, want)
	}
	if !strings.Contains(out, "smithy="+smithy) {
		t.Fatalf("the tool should inherit the resolved environment: %q", out)
	}
	if code != 5 {
		t.Fatalf("exit code = %d, want 5 (the tool's own)", code)
	}
}

func TestPyToolMissingCheckoutSaysWhereToClone(t *testing.T) {
	bin := buildPitf(t)
	smithy := t.TempDir()
	pathDir := t.TempDir()
	writeExec(t, pathDir, "uv", "#!/bin/sh\nexit 0\n")

	out, code := runPy(t, bin, pathDir, smithy, "forge", "--help")
	if code != 127 {
		t.Fatalf("missing checkout: exit %d, want 127\n%s", code, out)
	}
	if !strings.Contains(out, filepath.Join(smithy, "forge")) || !strings.Contains(out, "git clone https://github.com/erewhon/forge.git") {
		t.Fatalf("error should name the expected path and the clone URL: %q", out)
	}
}

func TestPyToolMissingUV(t *testing.T) {
	bin := buildPitf(t)
	smithy := t.TempDir()
	proj := filepath.Join(smithy, "meta")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(proj, "pyproject.toml"), []byte(""), 0o644); err != nil {
		t.Fatal(err)
	}
	out, code := runPy(t, bin, t.TempDir(), smithy, "meta")
	if code != 127 || !strings.Contains(out, "uv not on PATH") || !strings.Contains(out, "astral.sh/uv") {
		t.Fatalf("missing uv: code %d, %q", code, out)
	}
}

func TestPyToolBuiltinWinsOverStaleShim(t *testing.T) {
	bin := buildPitf(t)
	smithy := t.TempDir()
	pathDir := t.TempDir()
	writeExec(t, pathDir, "pitf-qual", "#!/bin/sh\necho SHIM\nexit 3\n")
	// No uv, no checkout: the built-in must still be what answers.
	out, code := runPy(t, bin, pathDir, smithy, "qual")
	if strings.Contains(out, "SHIM") || code == 3 {
		t.Fatalf("a pitf-qual shim on PATH must not shadow the built-in: code %d %q", code, out)
	}
	if code != 127 {
		t.Fatalf("expected the built-in's 127 for a missing checkout, got %d: %q", code, out)
	}
}

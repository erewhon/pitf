package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

var (
	buildOnce sync.Once
	builtBin  string
	buildErr  error
)

// buildPitf compiles the real binary once per test run so dispatch (which
// exec(2)s and never returns) can be observed from outside the process.
func buildPitf(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("exec semantics")
	}
	buildOnce.Do(func() {
		dir, err := os.MkdirTemp("", "pitf-test-bin")
		if err != nil {
			buildErr = err
			return
		}
		builtBin = filepath.Join(dir, "pitf")
		cmd := exec.Command("go", "build", "-o", builtBin, "../../cmd/pitf")
		if out, err := cmd.CombinedOutput(); err != nil {
			buildErr = err
			t.Logf("go build: %s", out)
		}
	})
	if buildErr != nil {
		t.Fatalf("go build: %v", buildErr)
	}
	return builtBin
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
	writeExec(t, dir, "pitf-hello", "#!/bin/sh\n")
	writeExec(t, dir, "pitf-qual", "#!/bin/sh\n") // shadowed by the built-in: not listed
	out, code := run(t, bin, dir, "help")
	if code != 0 || !strings.Contains(out, "External Commands") || !strings.Contains(out, "  hello") {
		t.Fatalf("help should list externals (code %d): %q", code, out)
	}
	if strings.Contains(out, "  qual\n") {
		t.Fatalf("help must not list an external a built-in shadows: %q", out)
	}
	out, code = run(t, bin, dir, "--version")
	if code != 0 || !strings.HasPrefix(out, "pitf ") {
		t.Fatalf("--version (code %d): %q", code, out)
	}
}

func TestDispatchAppliesConfigAndProfileFlag(t *testing.T) {
	bin := buildPitf(t)
	dir := t.TempDir()
	writeExec(t, dir, "pitf-show", "#!/bin/sh\necho \"url=$PITF_ROUTER_URL key=$ROUTER_API_KEY prof=$PITF_PROFILE extra=$EXTRA args=$*\"\n")
	cfgDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cfgDir, "pitf"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := `default_profile = "home"
[router]
url = "https://home.example"
api_key_cmd = "echo home-key"
[profiles.home]
[profiles.work.router]
url = "https://work.example"
api_key = "work-key"
[profiles.work.env]
EXTRA = "w"
`
	if err := os.WriteFile(filepath.Join(cfgDir, "pitf", "config.toml"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	pathEnv := dir + string(os.PathListSeparator) + os.Getenv("PATH")
	runCfg := func(args ...string) string {
		cmd := exec.Command(bin, args...)
		cmd.Env = append(os.Environ(), "PATH="+pathEnv, "XDG_CONFIG_HOME="+cfgDir, "ROUTER_API_KEY=", "PITF_PROFILE=")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	if got := runCfg("show", "a"); got != "url=https://home.example key=home-key prof=home extra= args=a" {
		t.Fatalf("default profile: %q", got)
	}
	// global flag before the external name selects the profile; the
	// external's own --profile-looking flag after the name is left alone.
	if got := runCfg("--profile", "work", "show", "--profile", "x"); got != "url=https://work.example key=work-key prof=work extra=w args=--profile x" {
		t.Fatalf("explicit profile: %q", got)
	}
	if got := runCfg("--router-url", "https://flag.example", "show"); !strings.HasPrefix(got, "url=https://flag.example ") {
		t.Fatalf("--router-url: %q", got)
	}
}

func TestConfigShowRedactsAndPathHonoursXDG(t *testing.T) {
	bin := buildPitf(t)
	cfgDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cfgDir, "pitf"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfgDir, "pitf", "config.toml"), []byte("[router]\nurl=\"https://h\"\napi_key=\"sekrit\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin, "config", "show")
	cmd.Env = append(os.Environ(), "XDG_CONFIG_HOME="+cfgDir, "ROUTER_API_KEY=", "PITF_PROFILE=")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("config show: %v\n%s", err, out)
	}
	if strings.Contains(string(out), "sekrit") || !strings.Contains(string(out), "router.key:  set") {
		t.Fatalf("show must redact: %s", out)
	}
	cmd = exec.Command(bin, "config", "path")
	cmd.Env = append(os.Environ(), "XDG_CONFIG_HOME="+cfgDir)
	out, _ = cmd.CombinedOutput()
	if strings.TrimSpace(string(out)) != filepath.Join(cfgDir, "pitf", "config.toml") {
		t.Fatalf("config path: %s", out)
	}
}

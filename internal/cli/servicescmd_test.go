package cli

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/erewhon/pitf/internal/services"
)

type cliLaunchd struct{ loaded map[string]bool }

func (f *cliLaunchd) Bootstrap(p string) error {
	f.loaded[strings.TrimSuffix(filepath.Base(p), ".plist")] = true
	return nil
}
func (f *cliLaunchd) Bootout(label string) error   { delete(f.loaded, label); return nil }
func (f *cliLaunchd) Kickstart(label string) error { return nil }
func (f *cliLaunchd) State(label string) (services.State, error) {
	return services.State{Loaded: f.loaded[label], Running: f.loaded[label]}, nil
}

// withFakeHost points the services commands at temp dirs and a fake
// launchctl, and returns the dirs.
func withFakeHost(t *testing.T) (services.Dirs, *cliLaunchd) {
	t.Helper()
	root := t.TempDir()
	dirs := services.Dirs{
		Agents: filepath.Join(root, "LaunchAgents"), Logs: filepath.Join(root, "Logs"),
		Replaced: filepath.Join(root, "replaced"),
	}
	l := &cliLaunchd{loaded: map[string]bool{}}
	oldHost, oldRead := hostManager, readPlist
	hostManager = func(out io.Writer) (services.Manager, error) {
		return services.Manager{Dirs: dirs, L: l, Out: out,
			Probe: func(context.Context, string) error { return nil }}, nil
	}
	t.Cleanup(func() { hostManager, readPlist = oldHost, oldRead })

	cfg := filepath.Join(root, "config.toml")
	if err := os.WriteFile(cfg, []byte("[profiles.work.router]\nurl = \"http://127.0.0.1:4010\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PITF_CONFIG", cfg)
	t.Setenv("PITF_PROFILE", "")
	t.Setenv("PITF_DASHBOARD_URL", "")
	return dirs, l
}

func runPitf(t *testing.T, args ...string) (string, error) {
	t.Helper()
	gf := &globalFlags{}
	root := NewRoot("test", gf)
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs(args)
	err := root.ExecuteContext(context.Background())
	return out.String(), err
}

func TestServicesInstallAdoptsLegacyRouter(t *testing.T) {
	dirs, l := withFakeHost(t)
	if err := os.MkdirAll(dirs.Agents, 0o755); err != nil {
		t.Fatal(err)
	}
	legacy := filepath.Join(dirs.Agents, "com.me.llm-router.plist")
	if err := os.WriteFile(legacy, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	l.loaded["com.me.llm-router"] = true
	readPlist = func(p string) (services.Agent, error) {
		if p == legacy {
			return services.Agent{Path: p, Label: "com.me.llm-router",
				Args: []string{"/opt/homebrew/bin/llm-router", "-models-yaml", "/work/models.yaml", "-addr", ":4010"},
				Env:  map[string]string{"AWS_BEARER_TOKEN_BEDROCK": "secret"}}, nil
		}
		return services.ReadPlist(p)
	}

	out, err := runPitf(t, "--profile", "work", "services", "install", "--ingest-arg=-regime=metered")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	for _, want := range []string{"adopting " + legacy, "environment: AWS_BEARER_TOKEN_BEDROCK", "replaced by pitf's: -addr :4010",
		"no [tools] dashboard_url", "router    installed"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "secret") {
		t.Fatalf("install printed a secret:\n%s", out)
	}
	if l.loaded["com.me.llm-router"] {
		t.Fatal("legacy router still loaded")
	}
	router, _ := os.ReadFile(dirs.PlistPath("router"))
	for _, want := range []string{"<string>--profile</string>\n\t\t<string>work</string>", "<string>/work/models.yaml</string>",
		"<string>127.0.0.1:4010</string>", "<key>AWS_BEARER_TOKEN_BEDROCK</key>"} {
		if !strings.Contains(string(router), want) {
			t.Errorf("router plist missing %q:\n%s", want, router)
		}
	}
	dash, _ := os.ReadFile(dirs.PlistPath("dashboard"))
	if !strings.Contains(string(dash), "<key>PITF_DASHBOARD_URL</key>\n\t\t<string>http://127.0.0.1:4011</string>") {
		t.Errorf("dashboard plist:\n%s", dash)
	}
	ingest, _ := os.ReadFile(dirs.PlistPath("ingest"))
	if !strings.Contains(string(ingest), "<string>-regime=metered</string>") {
		t.Errorf("ingest plist:\n%s", ingest)
	}

	// Re-run: the legacy is gone, so the adopted flags must come from the
	// user or be lost. They are lost here; the router plist changes.
	readPlist = services.ReadPlist
	out, err = runPitf(t, "--profile", "work", "services", "install", "--models-yaml", "/work/models.yaml")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if !strings.Contains(out, "tokens    unchanged") {
		t.Errorf("re-install:\n%s", out)
	}
}

func TestServicesInstallRefusesShellWrapper(t *testing.T) {
	dirs, _ := withFakeHost(t)
	if err := os.MkdirAll(dirs.Agents, 0o755); err != nil {
		t.Fatal(err)
	}
	legacy := filepath.Join(dirs.Agents, "com.me.router.plist")
	if err := os.WriteFile(legacy, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	readPlist = func(p string) (services.Agent, error) {
		return services.Agent{Path: p, Label: "com.me.router", Args: []string{"/bin/zsh", "-lc", "llm-router -addr :4010"}}, nil
	}
	out, err := runPitf(t, "services", "install")
	if err == nil || !strings.Contains(err.Error(), "--replace-legacy") {
		t.Fatalf("want refusal, got %v\n%s", err, out)
	}
	if _, statErr := os.Stat(legacy); statErr != nil {
		t.Fatal("refused install still moved the legacy plist")
	}
	if _, err := runPitf(t, "services", "install", "--replace-legacy"); err != nil {
		t.Fatal(err)
	}
	if _, statErr := os.Stat(filepath.Join(dirs.Replaced, "com.me.router.plist")); statErr != nil {
		t.Fatal("legacy plist not moved aside")
	}
}

func TestUpNoMonitor(t *testing.T) {
	_, l := withFakeHost(t)
	if _, err := runPitf(t, "up", "--no-monitor"); err == nil || !strings.Contains(err.Error(), "services install") {
		t.Fatalf("up before install: %v", err)
	}
	if _, err := runPitf(t, "services", "install"); err != nil {
		t.Fatal(err)
	}
	if _, err := runPitf(t, "services", "stop"); err != nil {
		t.Fatal(err)
	}
	out, err := runPitf(t, "up", "--no-monitor")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if len(l.loaded) != 4 || !strings.Contains(out, "router    started") || !strings.Contains(out, "✓ http://127.0.0.1:4010/health") {
		t.Fatalf("up:\n%s", out)
	}
}

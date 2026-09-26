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
	t.Setenv("HOME", root) // no real ~/.config/llm-router/router.env
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
	if _, err := os.Stat(dirs.PlistPath("dashboard")); err == nil {
		t.Error("the retired pitf dashboard agent must not be installed")
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
	if _, err := runPitf(t, "up", "--no-monitor", "--no-open"); err == nil || !strings.Contains(err.Error(), "services install") {
		t.Fatalf("up before install: %v", err)
	}
	if _, err := runPitf(t, "services", "install"); err != nil {
		t.Fatal(err)
	}
	if _, err := runPitf(t, "services", "stop"); err != nil {
		t.Fatal(err)
	}
	out, err := runPitf(t, "up", "--no-monitor", "--no-open")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if len(l.loaded) != 3 || !strings.Contains(out, "router    started") || !strings.Contains(out, "✓ http://127.0.0.1:4010/health") ||
		!strings.Contains(out, "dashboard http://127.0.0.1:4011/") {
		t.Fatalf("up:\n%s", out)
	}
}

func TestServicesInstallRouterEnvFile(t *testing.T) {
	dirs, _ := withFakeHost(t)
	home, _ := os.UserHomeDir()
	envDir := filepath.Join(home, ".config", "llm-router")
	if err := os.MkdirAll(envDir, 0o755); err != nil {
		t.Fatal(err)
	}
	def := filepath.Join(envDir, "router.env")
	if err := os.WriteFile(def, []byte("LOCAL_KEY=s3cret\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := runPitf(t, "--profile", "work", "services", "install")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if !strings.Contains(out, "env file  "+def+" (LOCAL_KEY)") || !strings.Contains(out, "readable by others") || strings.Contains(out, "s3cret") {
		t.Fatalf("install output:\n%s", out)
	}
	router, _ := os.ReadFile(dirs.PlistPath("router"))
	if !strings.Contains(string(router), "<string>--env-file</string>\n\t\t<string>"+def+"</string>\n\t\t<string>router</string>") ||
		strings.Contains(string(router), "s3cret") {
		t.Fatalf("router plist:\n%s", router)
	}

	bad := filepath.Join(envDir, "bad.env")
	if err := os.WriteFile(bad, []byte("not a line\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := runPitf(t, "services", "install", "--router-env-file", bad); err == nil || !strings.Contains(err.Error(), "bad.env:1") {
		t.Fatalf("bad env file: %v", err)
	}
}

func TestGlobalEnvFileBeforeAndAfterCommand(t *testing.T) {
	withFakeHost(t)
	t.Setenv("PITF_T_KEY", "")
	f := filepath.Join(t.TempDir(), "x.env")
	if err := os.WriteFile(f, []byte("PITF_T_KEY=from-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Main(context.Background(), "test", []string{"--env-file", f, "config", "path"}); err != nil {
		t.Fatal(err)
	}
	if os.Getenv("PITF_T_KEY") != "from-file" {
		t.Fatalf("before the command: %q", os.Getenv("PITF_T_KEY"))
	}
	os.Setenv("PITF_T_KEY", "")
	if err := Main(context.Background(), "test", []string{"config", "path", "--env-file", f}); err != nil {
		t.Fatal(err)
	}
	if os.Getenv("PITF_T_KEY") != "from-file" {
		t.Fatalf("after the command: %q", os.Getenv("PITF_T_KEY"))
	}
}

func TestServicesInstallFromConfigAndWellKnownDefault(t *testing.T) {
	dirs, _ := withFakeHost(t)
	home, _ := os.UserHomeDir()
	envFile := filepath.Join(home, "keys.env")
	if err := os.WriteFile(envFile, []byte("K=v\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := os.Getenv("PITF_CONFIG")
	if err := os.WriteFile(cfg, []byte(`[profiles.work.router]
url = "http://127.0.0.1:4010"

[profiles.work.services]
models_yaml = "~/work-models.yaml"
router_env_files = ["~/keys.env"]
router_args = ["-log-format=text", "-api-keys", "sk-hidden"]
ingest_every = "2m"
ingest_args = ["-regime=metered"]
`), 0o600); err != nil {
		t.Fatal(err)
	}

	out, err := runPitf(t, "--profile", "work", "services", "install")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if !strings.Contains(out, "settings  [services] / [profiles.work.services] in "+cfg) {
		t.Errorf("output:\n%s", out)
	}
	read := func(name string) string {
		b, _ := os.ReadFile(dirs.PlistPath(name))
		return string(b)
	}
	router := read("router")
	args := plistArgs(router)
	for _, want := range [][]string{
		{"--env-file", envFile, "router", "serve", "-models-yaml", filepath.Join(home, "work-models.yaml")},
		{"-wellknown-provider-id=llm", "-wellknown-base-url=http://127.0.0.1:4010/v1", "-log-format=text", "-api-keys", "sk-hidden"},
	} {
		if !containsSeq(args, want) {
			t.Fatalf("router args %q\nwant in order %q", args, want)
		}
	}
	if ingest := read("ingest"); !strings.Contains(ingest, "<integer>120</integer>") || !strings.Contains(ingest, "-regime=metered") {
		t.Errorf("ingest plist:\n%s", ingest)
	}

	// A bare re-run reproduces it exactly: nothing reloads.
	out, err = runPitf(t, "--profile", "work", "services", "install")
	if err != nil || strings.Count(out, "unchanged") != 3 {
		t.Fatalf("re-run should change nothing: %v\n%s", err, out)
	}

	// Flags: scalars override, lists append; a typed well-known id wins
	// over the default because it comes later.
	out, err = runPitf(t, "--profile", "work", "services", "install", "--router-addr", "127.0.0.1:4020",
		"--router-arg=-wellknown-provider-id=work", "--ingest-every", "1m")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	args = plistArgs(read("router"))
	if !containsSeq(args, []string{"-addr", "127.0.0.1:4020"}) ||
		!containsSeq(args, []string{"-wellknown-base-url=http://127.0.0.1:4020/v1", "-log-format=text", "-api-keys", "sk-hidden", "-wellknown-provider-id=work"}) {
		t.Fatalf("router args %q", args)
	}
	if !strings.Contains(read("ingest"), "<integer>60</integer>") {
		t.Error("--ingest-every should override the config")
	}

	show, err := runPitf(t, "--profile", "work", "config", "show")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(show, "router_args = -log-format=text -api-keys …") || strings.Contains(show, "sk-hidden") {
		t.Fatalf("config show:\n%s", show)
	}
}

func plistArgs(plist string) []string {
	start := strings.Index(plist, "<array>")
	end := strings.Index(plist, "</array>")
	var out []string
	for _, line := range strings.Split(plist[start:end], "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "<string>") {
			out = append(out, strings.TrimSuffix(strings.TrimPrefix(line, "<string>"), "</string>"))
		}
	}
	return out
}

// containsSeq reports whether want appears in args as a contiguous run.
func containsSeq(args, want []string) bool {
outer:
	for i := 0; i+len(want) <= len(args); i++ {
		for j := range want {
			if args[i+j] != want[j] {
				continue outer
			}
		}
		return true
	}
	return false
}

package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const sample = `
default_profile = "home"

[router]
url = "https://home.example"
api_key_cmd = "echo home-key"

[env]
SHARED = "from-defaults"
OVERRIDDEN = "from-defaults"

[profiles.home]

[profiles.work.router]
url = "https://work.example"
api_key = "work-literal"

[profiles.work.env]
OVERRIDDEN = "from-work"

[tools]
dashboard_url = "https://home.example/dashboard"

[nous]
url = "https://nous.example"
api_key_cmd = "echo nous-key"

[profiles.work.tools]
tokens_url = "http://work-box:8990"
`

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func envOf(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func mustResolve(t *testing.T, body string, opts Options) *Resolved {
	t.Helper()
	p := writeConfig(t, body)
	f, found, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("config not found")
	}
	getenv := opts.Getenv
	if getenv == nil {
		getenv = envOf(nil)
	}
	r, err := resolve(f, p, found, opts, getenv)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestPrecedenceTable(t *testing.T) {
	cases := []struct {
		name      string
		opts      Options
		env       map[string]string
		profile   string
		url       string
		keySource string
	}{
		{"file defaults + default_profile", Options{}, nil, "home", "https://home.example", "[router] defaults (command)"},
		{"profile from file", Options{Profile: "work"}, nil, "work", "https://work.example", "profile work (literal)"},
		{"PITF_PROFILE env", Options{}, map[string]string{"PITF_PROFILE": "work"}, "work", "https://work.example", "profile work (literal)"},
		{"flag beats PITF_PROFILE", Options{Profile: "home"}, map[string]string{"PITF_PROFILE": "work"}, "home", "https://home.example", "[router] defaults (command)"},
		{"PITF_ROUTER_URL beats profile", Options{Profile: "work"}, map[string]string{"PITF_ROUTER_URL": "https://env.example"}, "work", "https://env.example", "profile work (literal)"},
		{"--router-url beats PITF_ROUTER_URL", Options{RouterURL: "https://flag.example"}, map[string]string{"PITF_ROUTER_URL": "https://env.example"}, "home", "https://flag.example", "[router] defaults (command)"},
		{"ambient ROUTER_API_KEY honoured when profile implicit", Options{}, map[string]string{"ROUTER_API_KEY": "ambient"}, "home", "https://home.example", "ROUTER_API_KEY (already set)"},
		{"explicit profile beats ambient ROUTER_API_KEY", Options{Profile: "work"}, map[string]string{"ROUTER_API_KEY": "ambient"}, "work", "https://work.example", "profile work (literal)"},
		{"PITF_ROUTER_API_KEY beats everything", Options{Profile: "work"}, map[string]string{"PITF_ROUTER_API_KEY": "pitf-env", "ROUTER_API_KEY": "ambient"}, "work", "https://work.example", "PITF_ROUTER_API_KEY"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			c.opts.Getenv = envOf(c.env)
			r := mustResolve(t, sample, c.opts)
			if r.Profile != c.profile || r.RouterURL != c.url || r.KeySource != c.keySource {
				t.Fatalf("got profile=%q url=%q key=%q; want %q %q %q", r.Profile, r.RouterURL, r.KeySource, c.profile, c.url, c.keySource)
			}
		})
	}
}

func TestUnknownProfileIsAnError(t *testing.T) {
	p := writeConfig(t, sample)
	f, _, _ := Load(p)
	_, err := resolve(f, p, true, Options{Profile: "nope"}, envOf(nil))
	if err == nil || !strings.Contains(err.Error(), `profile "nope"`) || !strings.Contains(err.Error(), "home, work") {
		t.Fatalf("want a pointed unknown-profile error, got %v", err)
	}
}

func TestUnknownKeysRejected(t *testing.T) {
	p := writeConfig(t, "[router]\nurl = \"x\"\nurll = \"typo\"\n")
	if _, _, err := Load(p); err == nil || !strings.Contains(err.Error(), "router.urll") {
		t.Fatalf("typo'd key should be rejected, got %v", err)
	}
}

func TestMissingFileIsEmptyNotError(t *testing.T) {
	f, found, err := Load(filepath.Join(t.TempDir(), "absent.toml"))
	if err != nil || found || f.DefaultProfile != "" {
		t.Fatalf("missing file: found=%v err=%v", found, err)
	}
}

func TestAPIKeyCmdIsLazyAndRunsOnce(t *testing.T) {
	calls := 0
	r := mustResolve(t, sample, Options{
		Run: func(cmd string) (string, error) { calls++; return "  home-key\n", nil },
	})
	if calls != 0 {
		t.Fatal("api_key_cmd ran during Resolve; it must be lazy")
	}
	for i := 0; i < 2; i++ {
		k, err := r.APIKey()
		if err != nil || k != "home-key" {
			t.Fatalf("APIKey = %q, %v", k, err)
		}
	}
	if calls != 1 {
		t.Fatalf("api_key_cmd ran %d times, want exactly once", calls)
	}
}

func TestAPIKeyCmdNotRunWhenAmbientKeySatisfies(t *testing.T) {
	calls := 0
	r := mustResolve(t, sample, Options{
		Getenv: envOf(map[string]string{"ROUTER_API_KEY": "ambient"}),
		Run: func(cmd string) (string, error) {
			if cmd == "echo home-key" { // the router key command; the nous one may run
				calls++
			}
			return "x", nil
		},
	})
	env, err := r.Environment()
	if err != nil {
		t.Fatal(err)
	}
	if calls != 0 {
		t.Fatal("api_key_cmd ran although ROUTER_API_KEY was already set")
	}
	if !contains(env, "ROUTER_API_KEY=ambient") || !contains(env, "PITF_ROUTER_API_KEY=ambient") {
		t.Fatalf("env = %v", env)
	}
}

func TestEnvironmentMergesTablesAndRespectsAmbient(t *testing.T) {
	// implicit profile: hand-exported OVERRIDDEN survives, SHARED is added.
	r := mustResolve(t, sample, Options{
		Getenv: envOf(map[string]string{"OVERRIDDEN": "by-hand", "ROUTER_API_KEY": "k"}),
	})
	env, _ := r.Environment()
	if contains(env, "OVERRIDDEN=from-defaults") || !contains(env, "SHARED=from-defaults") {
		t.Fatalf("implicit profile env = %v", env)
	}
	// explicit profile: profile table wins over defaults and over hand exports.
	r = mustResolve(t, sample, Options{
		Profile: "work",
		Getenv:  envOf(map[string]string{"OVERRIDDEN": "by-hand"}),
	})
	env, _ = r.Environment()
	want := []string{
		"AGENT_MONITOR_TOKENS_URL=http://work-box:8990", "NOUS_API_KEY=nous-key", "NOUS_DAEMON_URL=https://nous.example", "OVERRIDDEN=from-work",
		"PITF_DASHBOARD_URL=https://home.example/dashboard", "PITF_MONITOR_URL=" + DefaultMonitorURL,
		"PITF_PROFILE=work", "PITF_ROUTER_API_KEY=work-literal", "PITF_ROUTER_URL=https://work.example",
		"PITF_TOKENS_URL=http://work-box:8990", "ROUTER_API_KEY=work-literal", "SHARED=from-defaults",
		"TOKENATOR_MONITOR_URL=" + DefaultMonitorURL,
	}
	if strings.Join(env, " ") != strings.Join(want, " ") {
		t.Fatalf("explicit profile env =\n %v\nwant\n %v", env, want)
	}
}

func TestDefaultPath(t *testing.T) {
	if got := DefaultPath(envOf(map[string]string{"PITF_CONFIG": "/x/y.toml"})); got != "/x/y.toml" {
		t.Fatal(got)
	}
	if got := DefaultPath(envOf(map[string]string{"XDG_CONFIG_HOME": "/xdg"})); got != "/xdg/pitf/config.toml" {
		t.Fatal(got)
	}
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func TestToolsMergeWithDefaults(t *testing.T) {
	r := mustResolve(t, sample, Options{})
	if r.Tools.MonitorURL != DefaultMonitorURL || r.Tools.TokensURL != DefaultTokensURL || r.Tools.DashboardURL != "https://home.example/dashboard" {
		t.Fatalf("home tools = %+v", r.Tools)
	}
	r = mustResolve(t, sample, Options{Profile: "work", Getenv: envOf(map[string]string{"PITF_MONITOR_URL": "http://env:1"})})
	if r.Tools.MonitorURL != "http://env:1" || r.Tools.TokensURL != "http://work-box:8990" || r.Tools.DashboardURL != "https://home.example/dashboard" {
		t.Fatalf("work tools = %+v", r.Tools)
	}
}

func TestToolEnvRespectsHandExports(t *testing.T) {
	r := mustResolve(t, sample, Options{Getenv: envOf(map[string]string{"AGENT_MONITOR_TOKENS_URL": "http://mine:1", "ROUTER_API_KEY": "k"})})
	env, _ := r.Environment()
	if contains(env, "AGENT_MONITOR_TOKENS_URL="+DefaultTokensURL) || !contains(env, "TOKENATOR_MONITOR_URL="+DefaultMonitorURL) {
		t.Fatalf("implicit profile must keep the hand export and still add the other: %v", env)
	}
}

func TestNousResolutionAndEnv(t *testing.T) {
	calls := 0
	r := mustResolve(t, sample, Options{Run: func(cmd string) (string, error) {
		if cmd == "echo nous-key" {
			calls++
			return "nous-key\n", nil
		}
		return "home-key", nil
	}})
	if !r.HasNous() || r.NousURL != "https://nous.example" || r.NousKeySource != "[nous] defaults (command)" {
		t.Fatalf("nous: %+v", r)
	}
	env, err := r.Environment()
	if err != nil || !contains(env, "NOUS_DAEMON_URL=https://nous.example") || !contains(env, "NOUS_API_KEY=nous-key") || calls != 1 {
		t.Fatalf("env=%v err=%v calls=%d", env, err, calls)
	}
	// ambient NOUS_API_KEY wins on an implicit profile and the command never runs
	calls = 0
	r = mustResolve(t, sample, Options{Getenv: envOf(map[string]string{"NOUS_API_KEY": "amb", "ROUTER_API_KEY": "k"}), Run: func(string) (string, error) { calls++; return "x", nil }})
	k, _ := r.NousAPIKey()
	if k != "amb" || calls != 0 {
		t.Fatalf("ambient: %q calls=%d", k, calls)
	}
	// no [nous] section → nothing exported
	r = mustResolve(t, "[router]\nurl=\"x\"\n", Options{})
	env, _ = r.Environment()
	for _, kv := range env {
		if strings.HasPrefix(kv, "NOUS_") {
			t.Fatalf("NOUS_* exported without a [nous] section: %v", env)
		}
	}
}

package services

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func testOpts() Options {
	return Options{
		Pitf: "/opt/homebrew/bin/pitf", Profile: "work",
		ModelsYAML: "/Users/me/.config/llm-router/models.yaml",
		RouterAddr: DefaultRouterAddr, DashboardAddr: DefaultDashboardAddr,
	}
}

func TestPlanArgs(t *testing.T) {
	o := testOpts()
	o.RouterArgs = []string{"-log-format", "text"}
	o.RouterEnv = map[string]string{"AWS_BEARER_TOKEN_BEDROCK": "k"}
	o.DashboardURL = "http://127.0.0.1:4011"
	specs := Plan(o)
	var names []string
	for _, s := range specs {
		names = append(names, s.Name)
	}
	if !reflect.DeepEqual(names, Names) {
		t.Fatalf("order %v, want %v", names, Names)
	}
	want := []string{"/opt/homebrew/bin/pitf", "--profile", "work", "router", "serve",
		"-models-yaml", o.ModelsYAML, "-addr", "127.0.0.1:4010", "-dashboard", "-dashboard-addr", "127.0.0.1:4011",
		"-log-format", "text"}
	if !reflect.DeepEqual(specs[0].Args, want) {
		t.Fatalf("router args\n got %q\nwant %q", specs[0].Args, want)
	}
	if specs[0].Env["AWS_BEARER_TOKEN_BEDROCK"] != "k" || specs[0].Env["PATH"] != DefaultPath {
		t.Fatalf("router env %v", specs[0].Env)
	}
	if _, leaked := specs[1].Env["AWS_BEARER_TOKEN_BEDROCK"]; leaked {
		t.Fatal("router secrets leaked into the tokens agent")
	}
	if specs[2].Env["PITF_DASHBOARD_URL"] != "http://127.0.0.1:4011" {
		t.Fatalf("dashboard env %v", specs[2].Env)
	}
	if specs[3].Interval != DefaultIngestEvery || specs[3].KeepAlive {
		t.Fatalf("ingest %+v", specs[3])
	}
	if specs[0].URL != "http://127.0.0.1:4010/health" {
		t.Fatalf("router url %s", specs[0].URL)
	}

	o.Profile = ""
	if got := Plan(o)[1].Args; !reflect.DeepEqual(got, []string{"/opt/homebrew/bin/pitf", "tokens", "serve"}) {
		t.Fatalf("no-profile tokens args %q", got)
	}
}

func TestPlistIsWellFormed(t *testing.T) {
	o := testOpts()
	o.RouterEnv = map[string]string{"K": `a<b&"c"`}
	for _, s := range Plan(o) {
		l := Plist(s, "/Users/me/Library/Logs/pitf")
		d := xml.NewDecoder(bytes.NewReader(l))
		d.Strict = true
		for {
			if _, err := d.Token(); errors.Is(err, io.EOF) {
				break
			} else if err != nil {
				t.Fatalf("%s: %v\n%s", s.Name, err, l)
			}
		}
		text := string(l)
		for _, want := range []string{"<string>" + Label(s.Name) + "</string>", "/Users/me/Library/Logs/pitf/" + s.Name + ".log"} {
			if !strings.Contains(text, want) {
				t.Errorf("%s plist missing %q", s.Name, want)
			}
		}
		if s.Name == "ingest" {
			if !strings.Contains(text, "<key>StartInterval</key>\n\t<integer>300</integer>") || strings.Contains(text, "KeepAlive") {
				t.Errorf("ingest plist:\n%s", text)
			}
		}
		if s.Name == "router" && !strings.Contains(text, "a&lt;b&amp;&#34;c&#34;") {
			t.Errorf("env not escaped:\n%s", text)
		}
	}
}

func TestAdopt(t *testing.T) {
	a := Agent{Path: "/L/com.me.llm-router.plist", Label: "com.me.llm-router",
		Args: []string{"/opt/homebrew/bin/llm-router", "-models-yaml", "/m.yaml", "-addr", ":4010",
			"-api-keys", "sk-x", "--dashboard", "-dashboard-addr=127.0.0.1:4011", "-log-format=text"},
		Env: map[string]string{"PATH": "/usr/bin", "AWS_BEARER_TOKEN_BEDROCK": "b"}}
	ad, err := Adopt(a)
	if err != nil {
		t.Fatal(err)
	}
	if ad.ModelsYAML != "/m.yaml" {
		t.Errorf("models %q", ad.ModelsYAML)
	}
	if want := []string{"-api-keys", "sk-x", "-log-format=text"}; !reflect.DeepEqual(ad.Args, want) {
		t.Errorf("args %q want %q", ad.Args, want)
	}
	if want := []string{"-addr :4010", "--dashboard", "-dashboard-addr=127.0.0.1:4011"}; !reflect.DeepEqual(ad.Dropped, want) {
		t.Errorf("dropped %q want %q", ad.Dropped, want)
	}
	if _, ok := ad.Env["PATH"]; ok || ad.Env["AWS_BEARER_TOKEN_BEDROCK"] != "b" {
		t.Errorf("env %v", ad.Env)
	}

	if _, err := Adopt(Agent{Path: "/w.plist", Args: []string{"/bin/sh", "-c", "exec llm-router -addr :4010"}}); err == nil {
		t.Error("shell wrapper adopted")
	}
}

func TestFindLegacy(t *testing.T) {
	dir := t.TempDir()
	agents := map[string]Agent{
		"com.me.llm-router.plist":    {Label: "com.me.llm-router", Args: []string{"/opt/homebrew/bin/llm-router", "-addr", ":4010"}},
		"com.me.node-agent.plist":    {Label: "com.me.node-agent", Args: []string{"/opt/homebrew/bin/node-agent"}},
		"com.me.wrapped.plist":       {Label: "com.me.wrapped", Args: []string{"/bin/zsh", "-lc", "llm-router -addr :4010"}},
		LabelPrefix + "router.plist": {Label: LabelPrefix + "router", Args: []string{"/opt/homebrew/bin/pitf", "router", "serve"}},
		"com.me.unrelated.plist":     {Label: "com.me.unrelated", Args: []string{"/usr/bin/true"}},
		"com.me.broken.plist":        {},
		"not-a-plist.txt":            {Label: "x", Args: []string{"llm-router"}},
	}
	for name := range agents {
		if err := os.WriteFile(filepath.Join(dir, name), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	read := func(p string) (Agent, error) {
		a, ok := agents[filepath.Base(p)]
		if !ok || a.Label == "" {
			return Agent{}, errors.New("unreadable")
		}
		a.Path = p
		return a, nil
	}
	got, err := FindLegacy(dir, read)
	if err != nil {
		t.Fatal(err)
	}
	var labels []string
	for _, a := range got {
		labels = append(labels, a.Label)
	}
	if want := []string{"com.me.llm-router", "com.me.wrapped"}; !reflect.DeepEqual(labels, want) {
		t.Fatalf("legacy %v want %v", labels, want)
	}
	if got, err := FindLegacy(filepath.Join(dir, "missing"), read); err != nil || got != nil {
		t.Fatalf("missing dir: %v %v", got, err)
	}
}

func TestParsePrint(t *testing.T) {
	st := parsePrint(`gui/501/org.erewhon.pitf.router = {
	active count = 1
	path = /Users/me/Library/LaunchAgents/org.erewhon.pitf.router.plist
	state = running
	program = /opt/homebrew/bin/pitf
	pid = 4242
	last exit code = 0
	endpoints = {
		"x" = {
			last exit code = 9
		}
	}
}`)
	if !st.Loaded || !st.Running || st.PID != 4242 || st.LastExit != "0" {
		t.Fatalf("%+v", st)
	}
}

func TestDecodePlistJSONProgram(t *testing.T) {
	a, err := decodePlistJSON("/p", []byte(`{"Label":"l","Program":"/opt/homebrew/bin/llm-router","ProgramArguments":["llm-router","-addr",":4010"],"EnvironmentVariables":{"K":"v"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"/opt/homebrew/bin/llm-router", "-addr", ":4010"}; !reflect.DeepEqual(a.Args, want) || a.Env["K"] != "v" {
		t.Fatalf("%+v", a)
	}
}

// fakeLaunchd records calls and keeps a loaded set.
type fakeLaunchd struct {
	loaded map[string]bool
	calls  []string
}

func (f *fakeLaunchd) Bootstrap(p string) error {
	label := strings.TrimSuffix(filepath.Base(p), ".plist")
	f.loaded[label] = true
	f.calls = append(f.calls, "bootstrap "+label)
	return nil
}
func (f *fakeLaunchd) Bootout(label string) error {
	if f.loaded[label] {
		f.calls = append(f.calls, "bootout "+label)
	}
	delete(f.loaded, label)
	return nil
}
func (f *fakeLaunchd) Kickstart(label string) error {
	f.calls = append(f.calls, "kickstart "+label)
	return nil
}
func (f *fakeLaunchd) State(label string) (State, error) {
	return State{Loaded: f.loaded[label], Running: f.loaded[label], PID: 1}, nil
}

func testManager(t *testing.T) (Manager, *fakeLaunchd, *bytes.Buffer) {
	root := t.TempDir()
	l := &fakeLaunchd{loaded: map[string]bool{}}
	out := &bytes.Buffer{}
	return Manager{Dirs: Dirs{
		Agents: filepath.Join(root, "LaunchAgents"), Logs: filepath.Join(root, "Logs"),
		Replaced: filepath.Join(root, "replaced"),
	}, L: l, Out: out}, l, out
}

func TestInstallRetiresLegacyAndIsIdempotent(t *testing.T) {
	m, l, out := testManager(t)
	if err := os.MkdirAll(m.Dirs.Agents, 0o755); err != nil {
		t.Fatal(err)
	}
	legacyPath := filepath.Join(m.Dirs.Agents, "com.me.llm-router.plist")
	if err := os.WriteFile(legacyPath, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	l.loaded["com.me.llm-router"] = true

	specs := Plan(testOpts())
	if err := m.Install(specs, &Agent{Path: legacyPath, Label: "com.me.llm-router"}); err != nil {
		t.Fatal(err)
	}
	if l.calls[0] != "bootout com.me.llm-router" || l.calls[1] != "bootstrap "+Label("router") {
		t.Fatalf("legacy must stop before the router starts: %v", l.calls)
	}
	if _, err := os.Stat(legacyPath); !os.IsNotExist(err) {
		t.Fatal("legacy plist still in LaunchAgents")
	}
	if b, err := os.ReadFile(filepath.Join(m.Dirs.Replaced, "com.me.llm-router.plist")); err != nil || string(b) != "old" {
		t.Fatalf("legacy plist not kept: %v", err)
	}
	if info, err := os.Stat(m.Dirs.PlistPath("router")); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("router plist: %v %v", info, err)
	}
	if got := m.Installed(); !reflect.DeepEqual(got, Names) {
		t.Fatalf("installed %v", got)
	}

	// Same specs again: nothing reloads.
	l.calls = nil
	out.Reset()
	if err := m.Install(specs, nil); err != nil {
		t.Fatal(err)
	}
	if len(l.calls) != 0 || strings.Count(out.String(), "unchanged") != 4 {
		t.Fatalf("re-install touched launchd: %v\n%s", l.calls, out)
	}

	// A changed router plist reloads only the router.
	o := testOpts()
	o.RouterArgs = []string{"-log-format", "text"}
	if err := m.Install(Plan(o), nil); err != nil {
		t.Fatal(err)
	}
	if want := []string{"bootout " + Label("router"), "bootstrap " + Label("router")}; !reflect.DeepEqual(l.calls, want) {
		t.Fatalf("calls %v want %v", l.calls, want)
	}
}

func TestUpStopUninstall(t *testing.T) {
	m, l, out := testManager(t)
	if err := m.Up(); !errors.Is(err, ErrNotInstalled) {
		t.Fatalf("up before install: %v", err)
	}
	if err := m.Install(Plan(testOpts()), nil); err != nil {
		t.Fatal(err)
	}
	if err := m.Stop(); err != nil {
		t.Fatal(err)
	}
	if len(l.loaded) != 0 {
		t.Fatalf("still loaded after stop: %v", l.loaded)
	}
	out.Reset()
	if err := m.Up(); err != nil {
		t.Fatal(err)
	}
	if len(l.loaded) != 4 || strings.Count(out.String(), "started") != 4 {
		t.Fatalf("up: %v\n%s", l.loaded, out)
	}

	probed := map[string]bool{}
	m.Probe = func(_ context.Context, url string) error {
		probed[url] = true
		if strings.Contains(url, "8990") {
			return errors.New("down")
		}
		return nil
	}
	out.Reset()
	if err := m.Status(context.Background(), Plan(testOpts())); err != nil {
		t.Fatal(err)
	}
	s := out.String()
	if !strings.Contains(s, "✓ http://127.0.0.1:4010/health") || !strings.Contains(s, "✗ http://127.0.0.1:8990/") {
		t.Fatalf("status:\n%s", s)
	}
	if err := m.WaitFor(context.Background(), "http://127.0.0.1:4010/health", time.Second); err != nil {
		t.Fatal(err)
	}

	if err := m.Uninstall(); err != nil {
		t.Fatal(err)
	}
	if len(m.Installed()) != 0 || len(l.loaded) != 0 {
		t.Fatalf("uninstall left %v / %v", m.Installed(), l.loaded)
	}
}

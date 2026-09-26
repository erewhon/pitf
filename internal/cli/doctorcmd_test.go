package cli

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/erewhon/pitf/internal/config"
)

func doctorResolve(t *testing.T, env map[string]string) *config.Resolved {
	t.Helper()
	getenv := func(k string) string { return env[k] }
	if _, ok := env["PITF_CONFIG"]; !ok {
		env["PITF_CONFIG"] = filepath.Join(t.TempDir(), "none.toml")
	}
	r, err := config.Resolve(config.Options{Getenv: getenv})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func byArea(cs []check, area string) []check {
	var out []check
	for _, c := range cs {
		if c.area == area {
			out = append(out, c)
		}
	}
	return out
}

func TestDoctorPythonChecksNameTheMissingCheckouts(t *testing.T) {
	smithy := t.TempDir()
	if err := os.MkdirAll(filepath.Join(smithy, "forge"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(smithy, "forge", "pyproject.toml"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	r := doctorResolve(t, map[string]string{"PITF_SMITHY_DIR": smithy})
	env := doctorEnv{lookPath: func(string) (string, error) { return "", errors.New("nope") }, offline: true}
	cs := checkPython(r, env)

	if got := byArea(cs, "uv"); len(got) != 1 || got[0].status != statusWarn || !strings.Contains(got[0].hint, "astral.sh/uv") {
		t.Fatalf("uv missing should warn with the install hint: %+v", got)
	}
	if got := byArea(cs, "pitf forge"); len(got) != 1 || got[0].status != statusOK {
		t.Fatalf("forge present should be ok: %+v", got)
	}
	for _, name := range []string{"pitf qual", "pitf meta"} {
		got := byArea(cs, name)
		if len(got) != 1 || got[0].status != statusWarn || !strings.Contains(got[0].hint, "git clone ") || !strings.Contains(got[0].hint, smithy) {
			t.Fatalf("%s missing should warn with a clone hint into %s: %+v", name, smithy, got)
		}
	}
}

func TestDoctorFlagsShadowedAndRetiredShims(t *testing.T) {
	dir := t.TempDir()
	writeExec(t, dir, "pitf-qual", "#!/bin/sh\n")
	writeExec(t, dir, "pitf-bench-py", "#!/bin/sh\n")
	writeExec(t, dir, "pitf-hello", "#!/bin/sh\n")
	root := NewRoot("test", nil)
	cs := checkExternals(doctorEnv{pathEnv: dir, root: root})
	if len(cs) != 3 {
		t.Fatalf("want 3 external checks, got %+v", cs)
	}
	want := map[string]checkStatus{"bench-py": statusWarn, "qual": statusWarn, "hello": statusInfo}
	for _, c := range cs {
		for name, st := range want {
			if strings.Contains(c.detail, "pitf-"+name) || strings.Contains(c.detail, "pitf "+name+" ") {
				if c.status != st {
					t.Errorf("%s: status %v, want %v (%+v)", name, c.status, st, c)
				}
				delete(want, name)
			}
		}
	}
	if len(want) != 0 {
		t.Fatalf("checks missing for %v in %+v", want, cs)
	}
	if !strings.Contains(cs[0].detail, "retired") { // sorted: bench-py first
		t.Fatalf("bench-py should be called out as retired: %+v", cs[0])
	}
}

func TestDoctorRouterProbeAndKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			w.WriteHeader(200)
		case "/v1/models":
			if r.Header.Get("Authorization") != "Bearer good" {
				w.WriteHeader(401)
				return
			}
			_, _ = w.Write([]byte(`{"data":[{"id":"a"},{"id":"b"}]}`))
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	env := doctorEnv{http: srv.Client(), lookPath: func(string) (string, error) { return "", errors.New("x") }}

	r := doctorResolve(t, map[string]string{"PITF_ROUTER_URL": srv.URL, "PITF_ROUTER_API_KEY": "good"})
	cs := checkRouter(context.Background(), r, env)
	if got := byArea(cs, "router"); len(got) != 1 || got[0].status != statusOK {
		t.Fatalf("router up: %+v", got)
	}
	if got := byArea(cs, "router.key"); len(got) != 1 || got[0].status != statusOK || !strings.Contains(got[0].detail, "2 models") {
		t.Fatalf("good key: %+v", got)
	}

	r = doctorResolve(t, map[string]string{"PITF_ROUTER_URL": srv.URL, "PITF_ROUTER_API_KEY": "bad"})
	cs = checkRouter(context.Background(), r, env)
	if got := byArea(cs, "router.key"); len(got) != 1 || got[0].status != statusFail || !strings.Contains(got[0].detail, "401") {
		t.Fatalf("bad key should FAIL with 401: %+v", got)
	}

	r = doctorResolve(t, map[string]string{"PITF_ROUTER_URL": "http://127.0.0.1:1"})
	cs = checkRouter(context.Background(), r, env)
	if got := byArea(cs, "router"); len(got) != 1 || got[0].status != statusFail || !strings.Contains(got[0].detail, "unreachable") {
		t.Fatalf("router down should FAIL: %+v", got)
	}
	if got := byArea(cs, "router.key"); len(got) != 1 || got[0].status != statusWarn {
		t.Fatalf("no key should warn: %+v", got)
	}

	// A tool UI answering 302 (SSO front door) counts as reachable.
	sso := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", "https://sso.example/")
		w.WriteHeader(302)
	}))
	defer sso.Close()
	c := sso.Client()
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	got := checkToolURL(context.Background(), doctorEnv{http: c}, "dashboard", sso.URL+"/", "hint")
	if got.status != statusOK || !strings.Contains(got.detail, "302") {
		t.Fatalf("302 should count as up: %+v", got)
	}
	got = checkToolURL(context.Background(), doctorEnv{http: c}, "tokens", "http://127.0.0.1:1/", "start it")
	if got.status != statusWarn || got.hint != "start it" {
		t.Fatalf("down tool should warn with the hint: %+v", got)
	}
}

// The dashboard's /tokens/ and /monitor/ proxies: 404 is the router saying
// "not configured" (with the flag to fix it), 502 is a configured proxy
// whose target is down, 200 is wired, a redirect is the SSO front door.
func TestDoctorDashProxies(t *testing.T) {
	dash := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/tokens/":
			w.WriteHeader(200)
		case "/monitor/api/agents":
			w.WriteHeader(404)
		}
	}))
	defer dash.Close()
	cs := checkDashProxies(context.Background(), doctorEnv{http: dash.Client()}, dash.URL+"/")
	if len(cs) != 2 {
		t.Fatalf("want 2 checks, got %+v", cs)
	}
	if cs[0].status != statusOK || !strings.Contains(cs[0].detail, "proxied") {
		t.Errorf("tokens proxied: %+v", cs[0])
	}
	if cs[1].status != statusWarn || !strings.Contains(cs[1].hint, "--dashboard-monitor-url") {
		t.Errorf("monitor unproxied should warn with the flag: %+v", cs[1])
	}
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(502) }))
	defer down.Close()
	cs = checkDashProxies(context.Background(), doctorEnv{http: down.Client()}, down.URL)
	if cs[0].status != statusWarn || !strings.Contains(cs[0].detail, "target is down") {
		t.Errorf("502 should read as target down: %+v", cs[0])
	}
	if cs := checkDashProxies(context.Background(), doctorEnv{offline: true}, "http://x"); cs != nil {
		t.Error("offline must skip the proxy probes")
	}
}

package dashboard

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/erewhon/pitf/internal/keys"
)

var links = keys.Links{
	MonitorURL:   "http://127.0.0.1:8070",
	TokensURL:    "http://127.0.0.1:8990/",
	DashboardURL: "https://llm.bcc.sh",
}

func urls(fs []Frame) map[string]string {
	out := map[string]string{}
	for _, f := range fs {
		out[f.ID] = f.URL
	}
	return out
}

func TestFramesFollowTheKeys(t *testing.T) {
	cases := []struct {
		name  string
		id    keys.SessionID
		alias keys.ModelAlias
		want  map[string]string
	}{
		{"no keys", "", "", map[string]string{
			"agents": "http://127.0.0.1:8070/",
			"tokens": "http://127.0.0.1:8990/",
			"router": "https://llm.bcc.sh/v2",
		}},
		{"session", "0d3e4b2a", "", map[string]string{
			"agents": "http://127.0.0.1:8070/",
			"tokens": "http://127.0.0.1:8990/session/0d3e4b2a",
			"router": "https://llm.bcc.sh/v2#requests?session=0d3e4b2a",
		}},
		{"model", "", "glm-fast", map[string]string{
			"agents": "http://127.0.0.1:8070/",
			"tokens": "http://127.0.0.1:8990/model/glm-fast",
			"router": "https://llm.bcc.sh/v2#catalog?model=glm-fast",
		}},
		{"session wins over model", "0d3e4b2a", "glm-fast", map[string]string{
			"agents": "http://127.0.0.1:8070/",
			"tokens": "http://127.0.0.1:8990/session/0d3e4b2a",
			"router": "https://llm.bcc.sh/v2#requests?session=0d3e4b2a",
		}},
	}
	for _, c := range cases {
		got := urls(Frames(links, c.id, c.alias))
		for k, want := range c.want {
			if got[k] != want {
				t.Errorf("%s: %s = %q, want %q", c.name, k, got[k], want)
			}
		}
	}
}

func TestUnconfiguredFramesExplainThemselves(t *testing.T) {
	for _, f := range Frames(keys.Links{}, "abcd", "x") {
		if f.URL != "" || !strings.Contains(f.Note, "not configured") || f.Hint == "" {
			t.Errorf("%s: want a not-configured note and no URL, got %+v", f.ID, f)
		}
	}
}

func TestRouterTabIsLinksNotAFrame(t *testing.T) {
	fs := Frames(links, "0d3e4b2a", "glm-fast")
	r := fs[2]
	if !r.LinkOnly || r.URL != "https://llm.bcc.sh/v2#requests?session=0d3e4b2a" {
		t.Fatalf("router tab: %+v", r)
	}
	var got []string
	for _, x := range r.Extra {
		got = append(got, x.URL)
	}
	want := []string{"https://llm.bcc.sh/v2#catalog?model=glm-fast", "https://llm.bcc.sh/v2"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("extra links %v, want %v", got, want)
	}
	if body := get(t, &Server{Links: links}, "/?session=0d3e4b2a"); strings.Contains(body, `data-src="https://llm.bcc.sh`) {
		t.Error("the router dashboard must not be framed")
	}
}

func TestLoopbackRouterDashboardIsFramed(t *testing.T) {
	local := keys.Links{MonitorURL: links.MonitorURL, TokensURL: links.TokensURL, DashboardURL: "http://127.0.0.1:4011"}
	r := Frames(local, "0d3e4b2a", "")[2]
	if r.LinkOnly || len(r.Extra) != 0 || r.URL != "http://127.0.0.1:4011/v2#requests?session=0d3e4b2a" {
		t.Fatalf("loopback router tab should be a plain frame: %+v", r)
	}
	body := get(t, &Server{Links: local}, "/?session=0d3e4b2a")
	if !strings.Contains(body, `data-src="http://127.0.0.1:4011/v2#requests?session=0d3e4b2a"`) {
		t.Error("loopback router dashboard should be framed")
	}
	for u, want := range map[string]bool{
		"http://localhost:4011": true, "http://[::1]:4011/": true, "http://127.0.0.1:4011": true,
		"https://llm-dashboard.bcc.sh": false, "http://192.168.1.5:4011": false, "": false,
	} {
		if got := isLoopbackURL(u); got != want {
			t.Errorf("isLoopbackURL(%q) = %v, want %v", u, got, want)
		}
	}
}

func TestLoopbackRouterIsProbed(t *testing.T) {
	local := keys.Links{DashboardURL: "http://127.0.0.1:4011"}
	s := &Server{Links: local, Probe: func(context.Context, string) error { return errors.New("connection refused") }}
	if body := get(t, s, "/?tab=router"); !strings.Contains(body, "start it with `pitf router serve`") {
		t.Error("a stopped loopback router should show a notice")
	}
}

func get(t *testing.T, s *Server, target string) string {
	t.Helper()
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s: %d", target, rec.Code)
	}
	b, _ := io.ReadAll(rec.Body)
	return string(b)
}

func TestPageFramesEachToolAndKeepsTheKeys(t *testing.T) {
	body := get(t, &Server{Links: links}, "/?session=0d3e4b2a&tab=router")
	for _, want := range []string{
		`data-src="http://127.0.0.1:8070/"`,
		`data-src="http://127.0.0.1:8990/session/0d3e4b2a"`,
		`href="https://llm.bcc.sh/v2#requests?session=0d3e4b2a" target="_blank"`,
		`href="https://llm.bcc.sh/v2" target="_blank" rel="noopener">dashboard home`,
		`id="panel-router" data-url="https://llm.bcc.sh/v2#requests?session=0d3e4b2a" class="active"`,
		`name="session" placeholder="session id" value="0d3e4b2a"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("page is missing %s", want)
		}
	}
}

func TestPageEscapesKeysAndDefaultsTheTab(t *testing.T) {
	body := get(t, &Server{Links: links}, `/?model=%22%3E%3Cscript%3E&tab=bogus`)
	if strings.Contains(body, `"><script>`) {
		t.Fatal("model alias was not escaped")
	}
	if !strings.Contains(body, `id="panel-agents" data-url="http://127.0.0.1:8070/" class="active"`) {
		t.Error("an unknown tab should fall back to agents")
	}
}

func TestProbeFailureReplacesTheLocalFramesOnly(t *testing.T) {
	var mu sync.Mutex
	var probed []string
	s := &Server{Links: links, Probe: func(_ context.Context, url string) error {
		mu.Lock() // the page probes its frames in parallel
		defer mu.Unlock()
		probed = append(probed, url)
		return errors.New("connection refused")
	}}
	body := get(t, s, "/")
	if strings.Contains(body, `data-src="http://127.0.0.1:8070/"`) || !strings.Contains(body, "start it with `pitf monitor`") {
		t.Error("an unreachable monitor should show a notice, not a frame")
	}
	if !strings.Contains(body, "start it with `pitf tokens serve`") {
		t.Error("an unreachable tokenator should show a notice")
	}
	if !strings.Contains(body, `href="https://llm.bcc.sh/v2" target="_blank"`) {
		t.Error("the router tab is never probed")
	}
	for _, u := range probed {
		if strings.HasPrefix(u, "https://llm.bcc.sh") {
			t.Errorf("probed the router: %s", u)
		}
	}
}

func TestCheckLoopback(t *testing.T) {
	for _, ok := range []string{"127.0.0.1:8960", "[::1]:8960", "localhost:0", "127.0.0.2:1"} {
		if err := CheckLoopback(ok); err != nil {
			t.Errorf("%s: %v", ok, err)
		}
	}
	for _, bad := range []string{":8960", "0.0.0.0:8960", "192.168.1.5:8960", "[::]:8960", "example.com:80", "nonsense"} {
		if err := CheckLoopback(bad); err == nil {
			t.Errorf("%s: want refusal", bad)
		}
	}
}

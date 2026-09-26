package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/erewhon/pitf/internal/bench"
	"github.com/erewhon/pitf/internal/keys"
)

func TestSessionJumps(t *testing.T) {
	l := keys.Links{MonitorURL: "http://m:8070", TokensURL: "http://t:8990", DashboardURL: "https://d/dashboard"}
	agents := []monitorAgent{
		{Name: "cc llm-router", Session: "llm-router", Target: "llm-router:0.0", Status: "running", SessionID: "0d3e4b2a-aaaa"},
		{Name: "cc meta", Session: "meta", Target: "meta:0.0", Status: "idle"},
	}
	var buf bytes.Buffer
	writeJumps(&buf, sessionJumps(l, "0d3e", agents, nil))
	out := buf.String()
	for _, want := range []string{"http://t:8990/session/0d3e", "http://t:8990/session/0d3e/transcript", "cc llm-router (tmux llm-router:0.0, running)", "http://m:8070/", "https://d/dashboard/v2#requests?session=0d3e", "https://d/dashboard/v2#tokens?session=0d3e"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	buf.Reset()
	writeJumps(&buf, sessionJumps(l, "ffff", agents, nil))
	if !strings.Contains(buf.String(), "no agent reports this session id") {
		t.Fatalf("unmatched id: %s", buf.String())
	}
	buf.Reset()
	writeJumps(&buf, sessionJumps(l, "0d3e", nil, errors.New("connection refused")))
	if !strings.Contains(buf.String(), "unreachable: connection refused") || !strings.Contains(buf.String(), "/session/0d3e") {
		t.Fatalf("monitor down must still print tokens links: %s", buf.String())
	}
	buf.Reset()
	writeJumps(&buf, sessionJumps(keys.Links{}, "0d3e", nil, nil))
	if !strings.Contains(buf.String(), "tokens_url not configured") || !strings.Contains(buf.String(), "monitor_url not configured") ||
		!strings.Contains(buf.String(), "dashboard_url not configured") {
		t.Fatalf("unconfigured: %s", buf.String())
	}
}

func TestModelJumps(t *testing.T) {
	l := keys.Links{DashboardURL: "https://d/dashboard", TokensURL: "http://t:8990"}
	models := []bench.Model{{ID: "glm-fast", OwnedBy: "vllm"}, {ID: "coder", Role: true}}
	var buf bytes.Buffer
	writeJumps(&buf, modelJumps(l, "glm-fast", models, nil))
	if !strings.Contains(buf.String(), "https://d/dashboard/v2#catalog?model=glm-fast") || !strings.Contains(buf.String(), "model served by vllm") ||
		!strings.Contains(buf.String(), "http://t:8990/model/glm-fast") || !strings.Contains(buf.String(), "https://d/dashboard/v2#tokens?model=glm-fast") {
		t.Fatalf("model: %s", buf.String())
	}
	buf.Reset()
	writeJumps(&buf, modelJumps(l, "coder", models, nil))
	if !strings.Contains(buf.String(), "role served by -") {
		t.Fatalf("role: %s", buf.String())
	}
	buf.Reset()
	writeJumps(&buf, modelJumps(l, "nope", models, nil))
	if !strings.Contains(buf.String(), `alias "nope" is not in /v1/models`) {
		t.Fatalf("missing alias: %s", buf.String())
	}
	buf.Reset()
	writeJumps(&buf, modelJumps(keys.Links{}, "x", nil, errors.New("timeout")))
	if !strings.Contains(buf.String(), "dashboard_url not configured") || !strings.Contains(buf.String(), "unreachable: timeout") ||
		!strings.Contains(buf.String(), "tokens_url not configured") {
		t.Fatalf("unconfigured + down: %s", buf.String())
	}
}

func TestFetchMonitorAgents(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/agents" {
			http.NotFound(w, r)
			return
		}
		json.NewEncoder(w).Encode([]monitorAgent{{Name: "a", Session: "s", Target: "s:0.0", Status: "idle", SessionID: "id-1"}})
	}))
	defer srv.Close()
	got, err := fetchMonitorAgents(context.Background(), keys.Links{MonitorURL: srv.URL}.MonitorAgentsAPI())
	if err != nil || len(got) != 1 || got[0].SessionID != "id-1" {
		t.Fatalf("got %v %v", got, err)
	}
	if got, err := fetchMonitorAgents(context.Background(), ""); got != nil || err != nil {
		t.Fatal("empty url must be a no-op")
	}
}

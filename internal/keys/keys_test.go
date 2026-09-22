package keys

import "testing"

func TestBuildersOmitUnconfiguredTargets(t *testing.T) {
	var none Links
	if none.MonitorBoard() != "" || none.TokensSession("abc") != "" || none.RouterCatalogModel("x") != "" {
		t.Fatal("unconfigured links must build empty URLs")
	}
	l := Links{MonitorURL: "http://127.0.0.1:8070/", TokensURL: "http://127.0.0.1:8990", DashboardURL: "https://llm.bcc.sh/dashboard/"}
	cases := map[string]string{
		l.MonitorBoard():                     "http://127.0.0.1:8070/",
		l.MonitorAgentsAPI():                 "http://127.0.0.1:8070/api/agents",
		l.TokensSession("0d3e4b2a"):          "http://127.0.0.1:8990/session/0d3e4b2a",
		l.TokensTranscript("0d3e4b2a"):       "http://127.0.0.1:8990/session/0d3e4b2a/transcript",
		l.RouterCatalogModel("glm-fast"):     "https://llm.bcc.sh/dashboard/v2#catalog?model=glm-fast",
		l.RouterCatalogModel("or/kimi k2.7"): "https://llm.bcc.sh/dashboard/v2#catalog?model=or%2Fkimi+k2.7",
		l.TokensSession("has/slash"):         "http://127.0.0.1:8990/session/has%2Fslash",
	}
	for got, want := range cases {
		if got != want {
			t.Errorf("got %q want %q", got, want)
		}
	}
	if l.TokensSession("") != "" {
		t.Fatal("empty id must not build a URL")
	}
}

func TestSessionPrefix(t *testing.T) {
	id := SessionID("0D3E")
	if !id.HasPrefix("0d3e4b2a-1111") || id.HasPrefix("1d3e") || SessionID("").HasPrefix("x") {
		t.Fatal("prefix match wrong")
	}
}

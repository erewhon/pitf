// Package keys holds the join keys the smithy tools share and the URL
// builders for jumping between them. It is the first shared type in pitf:
// when a tool itself needs to import it (rather than only pitf), that is
// the trigger to revisit the repo layout (see the PITF epic).
//
// Two keys exist today:
//
//   - SessionID: a harness session id (Claude Code's session UUID,
//     opencode's ses_… id). tokenator keys sessions on it (and accepts a
//     prefix); agent-monitor learns it from the hooks Claude Code runs and
//     reports it in /api/agents as session_id; the router logs the id the
//     harness sends on every request (llm-router-go f00737c+), and its
//     dashboard lists one session's requests (#requests?session=).
//   - ModelAlias: a router model alias. The router dashboard's catalog tab
//     deep-links on it, and tokenator's /model/{name} page matches it
//     against the alias the caller sent. agent-monitor only sees whatever
//     the harness called the model, so there is no alias join into it.
package keys

import (
	"net/url"
	"strings"
)

// SessionID is a harness session id (Claude Code session UUID or a prefix).
type SessionID string

// ModelAlias is a router alias as it appears in /v1/models.
type ModelAlias string

// Links is where each tool's UI lives. Empty means "not configured": the
// builders return "" and callers omit the jump.
type Links struct {
	MonitorURL   string // agent-monitor web UI (default port 8070)
	TokensURL    string // tokenator serve (default port 8990)
	DashboardURL string // router dashboard base (the listener that serves /v2)
}

func base(u string) string { return strings.TrimRight(u, "/") }

// MonitorBoard is the agent-monitor board.
func (l Links) MonitorBoard() string {
	if l.MonitorURL == "" {
		return ""
	}
	return base(l.MonitorURL) + "/"
}

// MonitorAgentsAPI is the JSON list the session lookup reads.
func (l Links) MonitorAgentsAPI() string {
	if l.MonitorURL == "" {
		return ""
	}
	return base(l.MonitorURL) + "/api/agents"
}

// TokensSession is tokenator's per-session profile. tokenator resolves a
// prefix to the most recently active matching session.
func (l Links) TokensSession(id SessionID) string {
	if l.TokensURL == "" || id == "" {
		return ""
	}
	return base(l.TokensURL) + "/session/" + url.PathEscape(string(id))
}

// TokensTranscript is the transcript view of the same session.
func (l Links) TokensTranscript(id SessionID) string {
	if s := l.TokensSession(id); s != "" {
		return s + "/transcript"
	}
	return ""
}

// RouterCatalogModel is the dashboard catalog tab opened on one alias.
func (l Links) RouterCatalogModel(alias ModelAlias) string {
	if l.DashboardURL == "" || alias == "" {
		return ""
	}
	return base(l.DashboardURL) + "/v2#catalog?model=" + url.QueryEscape(string(alias))
}

// TokensModel is tokenator's per-model page: the sessions that used the
// alias, and the router rows sent under it.
func (l Links) TokensModel(alias ModelAlias) string {
	if l.TokensURL == "" || alias == "" {
		return ""
	}
	return base(l.TokensURL) + "/model/" + url.PathEscape(string(alias))
}

// RouterSessionRequests is the dashboard's Requests tab opened on one
// session. The dashboard wants at least 4 characters of the id.
func (l Links) RouterSessionRequests(id SessionID) string {
	if l.DashboardURL == "" || id == "" {
		return ""
	}
	return base(l.DashboardURL) + "/v2#requests?session=" + url.QueryEscape(string(id))
}

// RouterTokensSession is the dashboard's Tokens tab (tokenator framed
// through the router's /tokens/ proxy) opened on one session — the one
// entry point when the dashboard is the home shell.
func (l Links) RouterTokensSession(id SessionID) string {
	if l.DashboardURL == "" || id == "" {
		return ""
	}
	return base(l.DashboardURL) + "/v2#tokens?session=" + url.QueryEscape(string(id))
}

// RouterTokensModel is the Tokens tab opened on tokenator's page for one
// model alias.
func (l Links) RouterTokensModel(alias ModelAlias) string {
	if l.DashboardURL == "" || alias == "" {
		return ""
	}
	return base(l.DashboardURL) + "/v2#tokens?model=" + url.QueryEscape(string(alias))
}

// HasPrefix reports whether candidate is the session (or starts with the
// typed prefix), case-insensitively. Both tools accept prefixes.
func (id SessionID) HasPrefix(candidate string) bool {
	if id == "" || candidate == "" {
		return false
	}
	return strings.HasPrefix(strings.ToLower(candidate), strings.ToLower(string(id)))
}

// Package dashboard serves `pitf dashboard`: one local page that frames the
// three tool UIs (agent-monitor, tokenator, the router dashboard) behind a
// shared nav, with the session id and model alias carried across them.
//
// It frames the running apps rather than mounting their handlers: tokenator's
// handler lives in an internal package, agent-monitor's mux is built inside
// its monitor loop, and both use absolute paths that break under a prefix.
// Framing needs no change to any of them.
//
// The page has no auth of its own, so it only listens on loopback. A router
// dashboard on loopback (the router's default --dashboard-addr, as on a work
// laptop) has no auth either and is framed like the others. A remote one is
// not: it sits behind the front door's SSO, the SSO cookie is not sent to a
// frame on a cross-site (loopback) page, and the login page refuses framing
// (frame-ancestors 'none'). Its tab is then a panel of links that open it in
// a new browser tab.
package dashboard

import (
	"context"
	_ "embed"
	"fmt"
	"html/template"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/erewhon/pitf/internal/keys"
)

//go:embed page.html
var pageHTML string

var pageTmpl = template.Must(template.New("page").Parse(pageHTML))

// DefaultListen is where `pitf dashboard` listens unless told otherwise.
const DefaultListen = "127.0.0.1:8960"

// Frame is one tab: the URL its iframe shows, or the reason it cannot.
type Frame struct {
	ID    string // DOM id and ?tab= value
	Label string
	URL   string
	Note  string // shown instead of the iframe when set
	Hint  string // extra line under a note (how to fix it)
	// LinkOnly marks a target that cannot be framed; the tab shows links
	// (URL plus Extra) that open in a new browser tab.
	LinkOnly bool
	URLLabel string
	Extra    []Link
}

// Link is one labelled URL on a link-only tab.
type Link struct {
	Label string
	URL   string
}

// Frames picks each tab's URL for the given keys. A session id takes
// precedence over a model alias for the tokens and router tabs; the monitor
// has no per-session view, so it always shows its board.
func Frames(l keys.Links, id keys.SessionID, alias keys.ModelAlias) []Frame {
	agents := Frame{ID: "agents", Label: "Agents", URL: l.MonitorBoard()}
	if agents.URL == "" {
		agents.Note, agents.Hint = "monitor_url not configured", "set [tools].monitor_url in the pitf config"
	}

	tokens := Frame{ID: "tokens", Label: "Sessions"}
	switch {
	case l.TokensURL == "":
		tokens.Note, tokens.Hint = "tokens_url not configured", "set [tools].tokens_url in the pitf config"
	case id != "":
		tokens.URL = l.TokensSession(id)
	case alias != "":
		tokens.URL = l.TokensModel(alias)
	default:
		tokens.URL = strings.TrimRight(l.TokensURL, "/") + "/"
	}

	router := Frame{ID: "router", Label: "Router", LinkOnly: !isLoopbackURL(l.DashboardURL)}
	switch {
	case l.DashboardURL == "":
		router.Note, router.Hint = "dashboard_url not configured", "set [tools].dashboard_url in the pitf config"
	case id != "":
		router.URL, router.URLLabel = l.RouterSessionRequests(id), "Router requests for session "+string(id)
	case alias != "":
		router.URL, router.URLLabel = l.RouterCatalogModel(alias), "Router catalog for "+string(alias)
	default:
		router.URL, router.URLLabel = strings.TrimRight(l.DashboardURL, "/")+"/v2", "Router dashboard"
	}
	if router.URL != "" && router.LinkOnly {
		home := strings.TrimRight(l.DashboardURL, "/") + "/v2"
		router.Extra = append(router.Extra, Link{"requests for this session", l.RouterSessionRequests(id)},
			Link{"catalog for this model", l.RouterCatalogModel(alias)}, Link{"dashboard home", home})
		// Drop empties and the entry the main link already covers.
		kept := router.Extra[:0]
		for _, x := range router.Extra {
			if x.URL != "" && x.URL != router.URL {
				kept = append(kept, x)
			}
		}
		router.Extra = kept
	}
	return []Frame{agents, tokens, router}
}

// Server renders the page. Probe, when set, checks a local target before it
// is framed so a stopped tool shows a notice instead of a browser error page.
type Server struct {
	Links keys.Links
	Probe func(ctx context.Context, url string) error
}

// Handler returns the route table: the page at / and nothing else.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.handlePage)
	return mux
}

type pageData struct {
	Session string
	Model   string
	Tab     string
	Frames  []Frame
}

func (s *Server) handlePage(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	id := keys.SessionID(strings.TrimSpace(q.Get("session")))
	alias := keys.ModelAlias(strings.TrimSpace(q.Get("model")))
	frames := Frames(s.Links, id, alias)
	s.probeLocal(r.Context(), frames)

	tab := q.Get("tab")
	valid := false
	for _, f := range frames {
		valid = valid || f.ID == tab
	}
	if !valid {
		tab = frames[0].ID
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_ = pageTmpl.Execute(w, pageData{Session: string(id), Model: string(alias), Tab: tab, Frames: frames})
}

// probeLocal checks the framed targets in parallel. Link-only targets are
// not probed: a remote router dashboard answers pitf with an SSO redirect,
// which says nothing about the browser's session.
func (s *Server) probeLocal(ctx context.Context, frames []Frame) {
	if s.Probe == nil {
		return
	}
	var wg sync.WaitGroup
	for i := range frames {
		f := &frames[i]
		if f.LinkOnly || f.URL == "" {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := s.Probe(ctx, f.URL); err != nil {
				f.Note = "unreachable: " + err.Error()
				switch f.ID {
				case "agents":
					f.Hint = "start it with `pitf monitor`"
				case "tokens":
					f.Hint = "start it with `pitf tokens serve`"
				case "router":
					f.Hint = "start it with `pitf router serve` (the dashboard listens on --dashboard-addr)"
				}
				f.URL = ""
			}
		}()
	}
	wg.Wait()
}

// HTTPProbe is the default Probe: any HTTP answer means the tool is up.
func HTTPProbe(ctx context.Context, target string) error {
	ctx, cancel := context.WithTimeout(ctx, 1500*time.Millisecond)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// isLoopbackURL reports whether u points at this machine (localhost or a
// loopback IP), where no front door or SSO sits in the way.
func isLoopbackURL(u string) bool {
	parsed, err := url.Parse(u)
	if err != nil {
		return false
	}
	host := parsed.Hostname()
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// CheckLoopback refuses listen addresses other than loopback: the page has
// no auth, and it hands out links into tools that have none either.
func CheckLoopback(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("listen address %q: %w", addr, err)
	}
	if host == "localhost" {
		return nil
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return nil
	}
	return fmt.Errorf("listen address %q is not loopback; pitf dashboard has no auth and only listens on 127.0.0.1, ::1 or localhost", addr)
}

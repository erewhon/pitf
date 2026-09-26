package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/erewhon/pitf/internal/bench"
	"github.com/erewhon/pitf/internal/config"
	"github.com/erewhon/pitf/internal/keys"
)

// monitorAgent is the subset of agent-monitor's /api/agents entry we use.
type monitorAgent struct {
	Name      string `json:"name"`
	Session   string `json:"session"` // tmux session name
	Target    string `json:"target"`  // tmux target session:window.pane
	Status    string `json:"status"`
	SessionID string `json:"session_id,omitempty"` // Claude Code session UUID, when a hook reported it
}

func fetchMonitorAgents(ctx context.Context, url string) ([]monitorAgent, error) {
	if url == "" {
		return nil, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := (&http.Client{Timeout: 3 * time.Second}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return nil, fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	var out []monitorAgent
	return out, json.NewDecoder(resp.Body).Decode(&out)
}

// jump is one place a key can be opened.
type jump struct {
	Tool string
	What string
	URL  string
	Note string // rendered instead of a URL when the jump is unavailable
}

// sessionJumps builds every jump for a session id. agents may be nil (monitor
// unreachable) — the monitor jump then explains itself instead of failing.
func sessionJumps(l keys.Links, id keys.SessionID, agents []monitorAgent, monitorErr error) []jump {
	var out []jump
	if u := l.TokensSession(id); u != "" {
		out = append(out, jump{Tool: "tokens", What: "profile", URL: u})
		out = append(out, jump{Tool: "tokens", What: "transcript", URL: l.TokensTranscript(id)})
	} else {
		out = append(out, jump{Tool: "tokens", Note: "tokens_url not configured"})
	}
	switch {
	case l.MonitorURL == "":
		out = append(out, jump{Tool: "monitor", Note: "monitor_url not configured"})
	case monitorErr != nil:
		out = append(out, jump{Tool: "monitor", Note: "unreachable: " + monitorErr.Error()})
	default:
		var hits []monitorAgent
		for _, a := range agents {
			if id.HasPrefix(a.SessionID) {
				hits = append(hits, a)
			}
		}
		if len(hits) == 0 {
			out = append(out, jump{Tool: "monitor", Note: "no agent reports this session id (hooks must send session_id; see agent-monitor hooks install)"})
		}
		for _, a := range hits {
			out = append(out, jump{Tool: "monitor", What: fmt.Sprintf("%s (tmux %s, %s)", a.Name, a.Target, a.Status), URL: l.MonitorBoard()})
		}
	}
	if u := l.RouterSessionRequests(id); u != "" {
		out = append(out, jump{Tool: "router", What: "requests", URL: u})
		out = append(out, jump{Tool: "router", What: "tokens tab", URL: l.RouterTokensSession(id)})
	} else {
		out = append(out, jump{Tool: "router", Note: "dashboard_url not configured (set [tools].dashboard_url)"})
	}
	return out
}

// modelJumps builds the jumps for a router alias. models may be nil when the
// router was not consulted.
func modelJumps(l keys.Links, alias keys.ModelAlias, models []bench.Model, routerErr error) []jump {
	var out []jump
	if u := l.RouterCatalogModel(alias); u != "" {
		out = append(out, jump{Tool: "router", What: "catalog", URL: u})
		out = append(out, jump{Tool: "router", What: "tokens tab", URL: l.RouterTokensModel(alias)})
	} else {
		out = append(out, jump{Tool: "router", Note: "dashboard_url not configured (set [tools].dashboard_url)"})
	}
	switch {
	case routerErr != nil:
		out = append(out, jump{Tool: "router", Note: "/v1/models unreachable: " + routerErr.Error()})
	case models != nil:
		found := false
		for _, m := range models {
			if m.ID == string(alias) {
				found = true
				kind := "model"
				switch {
				case m.Role:
					kind = "role"
				case m.Discovered:
					kind = "discovered"
				}
				out = append(out, jump{Tool: "router", What: fmt.Sprintf("%s served by %s", kind, orDash(m.OwnedBy))})
			}
		}
		if !found {
			out = append(out, jump{Tool: "router", Note: fmt.Sprintf("alias %q is not in /v1/models", alias)})
		}
	}
	if u := l.TokensModel(alias); u != "" {
		out = append(out, jump{Tool: "tokens", What: "model", URL: u})
	} else {
		out = append(out, jump{Tool: "tokens", Note: "tokens_url not configured"})
	}
	return out
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func writeJumps(w io.Writer, jumps []jump) {
	for _, j := range jumps {
		switch {
		case j.URL != "" && j.What != "":
			fmt.Fprintf(w, "%-8s %-40s %s\n", j.Tool, j.What, j.URL)
		case j.URL != "":
			fmt.Fprintf(w, "%-8s %s\n", j.Tool, j.URL)
		case j.What != "":
			fmt.Fprintf(w, "%-8s %s\n", j.Tool, j.What)
		default:
			fmt.Fprintf(w, "%-8s (%s)\n", j.Tool, j.Note)
		}
	}
}

func openURLs(jumps []jump) error {
	var opened int
	for _, j := range jumps {
		if j.URL == "" {
			continue
		}
		if err := openInBrowser(j.URL); err != nil {
			return err
		}
		opened++
	}
	if opened == 0 {
		return errors.New("nothing to open")
	}
	return nil
}

func openInBrowser(url string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	return cmd.Start()
}

func linksFor(r *config.Resolved) keys.Links {
	return keys.Links{MonitorURL: r.Tools.MonitorURL, TokensURL: r.Tools.TokensURL, DashboardURL: r.Tools.DashboardURL}
}

func newSessionCmd(gf *globalFlags) *cobra.Command {
	var open bool
	cmd := &cobra.Command{
		Use:   "session [id-or-prefix]",
		Short: "Jump to one coding session in tokenator, agent-monitor and the router",
		Long: "With an id (or a unique prefix of the harness session id), prints the\n" +
			"tokenator profile and transcript URLs, the agent-monitor agent that reports\n" +
			"the same id, and the router dashboard's requests for it (4+ characters).\n" +
			"With no id, lists the agents agent-monitor knows and their session ids.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			r, err := config.Resolve(gf.options())
			if err != nil {
				return err
			}
			l := linksFor(r)
			ctx := cmd.Context()
			if ctx == nil {
				ctx = context.Background()
			}
			agents, merr := fetchMonitorAgents(ctx, l.MonitorAgentsAPI())
			w := cmd.OutOrStdout()
			if len(args) == 0 {
				if l.MonitorURL == "" {
					return errors.New("monitor_url not configured; pass a session id to build tokenator links only")
				}
				if merr != nil {
					return fmt.Errorf("agent-monitor at %s: %w", l.MonitorURL, merr)
				}
				sort.Slice(agents, func(i, j int) bool { return agents[i].Name < agents[j].Name })
				fmt.Fprintf(w, "%-24s %-22s %-10s %s\n", "AGENT", "TMUX", "STATUS", "SESSION ID")
				for _, a := range agents {
					fmt.Fprintf(w, "%-24s %-22s %-10s %s\n", a.Name, a.Target, a.Status, orDash(a.SessionID))
				}
				return nil
			}
			jumps := sessionJumps(l, keys.SessionID(args[0]), agents, merr)
			writeJumps(w, jumps)
			if open {
				return openURLs(jumps)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&open, "open", false, "open every URL in the browser")
	return cmd
}

func newModelCmd(gf *globalFlags) *cobra.Command {
	var open bool
	cmd := &cobra.Command{
		Use:   "model <alias>",
		Short: "Jump to one router model alias in the dashboard and tokenator",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			r, err := config.Resolve(gf.options())
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			if ctx == nil {
				ctx = context.Background()
			}
			var models []bench.Model
			var rerr error
			if r.RouterURL != "" {
				key, kerr := r.APIKey()
				if kerr != nil {
					rerr = kerr
				} else {
					c := &bench.Client{BaseURL: r.RouterURL, APIKey: key, HTTP: &http.Client{Timeout: 5 * time.Second}}
					models, rerr = c.ListModels(ctx)
				}
			}
			jumps := modelJumps(linksFor(r), keys.ModelAlias(args[0]), models, rerr)
			writeJumps(cmd.OutOrStdout(), jumps)
			if open {
				return openURLs(jumps)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&open, "open", false, "open every URL in the browser")
	return cmd
}

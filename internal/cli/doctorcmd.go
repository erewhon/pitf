package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/erewhon/pitf/internal/bench"
	"github.com/erewhon/pitf/internal/config"
)

// pitf doctor: one pass over everything pitf needs and cannot know is there
// until it is used — the config file, the router and its key, the tool UIs
// the jumps point at, uv and the Python checkouts, tmux, and stale pitf-*
// shims on PATH. Each line is one check; a hint says how to fix it.

type checkStatus int

const (
	statusOK checkStatus = iota
	statusInfo
	statusWarn
	statusFail
)

func (s checkStatus) String() string {
	switch s {
	case statusOK:
		return "ok"
	case statusInfo:
		return "info"
	case statusWarn:
		return "warn"
	default:
		return "FAIL"
	}
}

type check struct {
	status checkStatus
	area   string // config, router, tokens, …
	detail string
	hint   string // how to fix it; empty when nothing to do
}

// doctorEnv is everything the checks touch outside the config, injectable
// for tests.
type doctorEnv struct {
	pathEnv    string
	lookPath   func(string) (string, error)
	http       *http.Client
	offline    bool   // skip network probes
	executable string // the running binary, for the PATH check
	version    string
	root       *cobra.Command // for built-in names (shadowed externals)
}

func defaultDoctorEnv(root *cobra.Command, version string, offline bool) doctorEnv {
	exe, _ := os.Executable()
	return doctorEnv{
		pathEnv:  os.Getenv("PATH"),
		lookPath: exec.LookPath,
		http: &http.Client{
			Timeout: 4 * time.Second,
			// A front proxy answering 302 to SSO is "reachable"; do not chase it.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		offline:    offline,
		executable: exe,
		version:    version,
		root:       root,
	}
}

func newDoctorCmd(version string, gf *globalFlags) *cobra.Command {
	var offline bool
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Check what pitf needs and say how to fix what is missing",
		Long: "Checks the config file, the router and its key, the tool UIs that pitf\n" +
			"session / pitf model jump to, uv and the Python checkouts behind pitf\n" +
			"qual / forge / meta, tmux for pitf monitor, and stale pitf-* shims on\n" +
			"PATH. Exit status 1 when anything FAILs; warnings do not fail.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			r, err := config.Resolve(gf.options())
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			if ctx == nil {
				ctx = context.Background()
			}
			checks := runDoctor(ctx, r, defaultDoctorEnv(cmd.Root(), version, offline))
			writeChecks(cmd.OutOrStdout(), checks)
			for _, c := range checks {
				if c.status == statusFail {
					return &ExitError{Code: 1}
				}
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&offline, "offline", false, "skip the network probes (router, tool UIs, nous)")
	return cmd
}

func writeChecks(w io.Writer, checks []check) {
	for _, c := range checks {
		fmt.Fprintf(w, "%-5s %-12s %s\n", c.status, c.area, c.detail)
		if c.hint != "" {
			fmt.Fprintf(w, "%-5s %-12s → %s\n", "", "", c.hint)
		}
	}
}

func runDoctor(ctx context.Context, r *config.Resolved, env doctorEnv) []check {
	var out []check
	add := func(s checkStatus, area, detail, hint string) {
		out = append(out, check{status: s, area: area, detail: detail, hint: hint})
	}

	// pitf itself.
	out = append(out, checkPitfOnPath(env)...)

	// Config file and profile.
	if r.FileFound {
		add(statusOK, "config", r.Path, "")
	} else {
		add(statusWarn, "config", "no config file at "+r.Path, "pitf config init writes a starter")
	}
	if r.Profile != "" {
		add(statusInfo, "profile", fmt.Sprintf("%s (from %s)", r.Profile, r.ProfileSource), "")
	}

	// Router URL, key, and whether the key is accepted.
	out = append(out, checkRouter(ctx, r, env)...)

	// Tool UIs the jumps point at.
	out = append(out, checkToolURL(ctx, env, "monitor", strings.TrimRight(r.Tools.MonitorURL, "/")+"/api/agents",
		"start it with `pitf monitor` (or `pitf up`), or set [tools].monitor_url"))
	out = append(out, checkToolURL(ctx, env, "tokens", strings.TrimRight(r.Tools.TokensURL, "/")+"/",
		"start it with `pitf tokens serve`, or point [tools].tokens_url at a running instance"))
	if r.Tools.DashboardURL == "" {
		add(statusInfo, "dashboard", "dashboard_url unset; pitf session / pitf model print no router links", "set [tools].dashboard_url to the router dashboard's public URL")
	} else {
		out = append(out, checkToolURL(ctx, env, "dashboard", strings.TrimRight(r.Tools.DashboardURL, "/")+"/", "check [tools].dashboard_url"))
		out = append(out, checkDashProxies(ctx, env, r.Tools.DashboardURL)...)
	}
	if r.HasNous() {
		out = append(out, checkToolURL(ctx, env, "nous", strings.TrimRight(r.NousURL, "/")+"/", "only `pitf bench import` needs it; check [nous].url"))
	} else {
		add(statusInfo, "nous", "unset (only `pitf bench import` needs it)", "")
	}

	// Python tools: uv + checkouts.
	out = append(out, checkPython(r, env)...)

	// tmux for pitf monitor.
	if p, err := env.lookPath("tmux"); err == nil {
		add(statusOK, "tmux", p, "")
	} else {
		add(statusWarn, "tmux", "not on PATH; `pitf monitor` needs it", "brew install tmux / apt install tmux")
	}

	// Externals: stale shims and the ones that still run.
	out = append(out, checkExternals(env)...)
	return out
}

func checkPitfOnPath(env doctorEnv) []check {
	if env.executable == "" {
		return nil
	}
	self, err := filepath.EvalSymlinks(env.executable)
	if err != nil {
		self = env.executable
	}
	p, err := env.lookPath("pitf")
	if err != nil {
		return []check{{statusWarn, "pitf", fmt.Sprintf("%s (%s) is not on PATH", self, env.version), "just install puts it in ~/.local/bin"}}
	}
	onPath, err := filepath.EvalSymlinks(p)
	if err != nil {
		onPath = p
	}
	if onPath != self {
		return []check{{statusWarn, "pitf", fmt.Sprintf("running %s (%s) but PATH resolves pitf to %s", self, env.version, onPath), "one of them is stale; `just install` or fix PATH order"}}
	}
	return []check{{statusOK, "pitf", fmt.Sprintf("%s (%s)", self, env.version), ""}}
}

func checkRouter(ctx context.Context, r *config.Resolved, env doctorEnv) []check {
	var out []check
	if r.RouterURL == "" {
		out = append(out, check{statusFail, "router", "router.url unset", "set [router].url (or a profile's), or pass --router-url"})
	} else if env.offline {
		out = append(out, check{statusInfo, "router", r.RouterURL + " (not probed: --offline)", ""})
	} else {
		st, err := probe(ctx, env.http, strings.TrimRight(r.RouterURL, "/")+"/health", "")
		switch {
		case err != nil:
			out = append(out, check{statusFail, "router", fmt.Sprintf("%s unreachable: %v", r.RouterURL, err), "is the router up? `pitf router serve` runs a local one"})
		case st >= 500:
			out = append(out, check{statusFail, "router", fmt.Sprintf("%s /health HTTP %d", r.RouterURL, st), "the router is up but unhealthy; check its logs"})
		default:
			out = append(out, check{statusOK, "router", fmt.Sprintf("%s (health %d, from %s)", r.RouterURL, st, r.RouterURLSource), ""})
		}
	}

	switch {
	case !r.HasKey():
		out = append(out, check{statusWarn, "router.key", "no key configured; bench, model and the mounted tools will be anonymous", "set [router].api_key_cmd (e.g. `ho secret get llm-router/api-key`)"})
		return out
	}
	key, err := r.APIKey()
	if err != nil {
		out = append(out, check{statusFail, "router.key", fmt.Sprintf("key from %s failed: %v", r.KeySource, err), "run the api_key_cmd by hand"})
		return out
	}
	if r.RouterURL == "" || env.offline {
		out = append(out, check{statusOK, "router.key", "resolved from " + r.KeySource, ""})
		return out
	}
	c := &bench.Client{BaseURL: r.RouterURL, APIKey: key, HTTP: env.http}
	models, err := c.ListModels(ctx)
	switch {
	case err != nil && strings.Contains(err.Error(), "401"):
		out = append(out, check{statusFail, "router.key", fmt.Sprintf("%s rejected the key from %s (401)", r.RouterURL, r.KeySource), "the key is wrong or revoked; mint a PAT in the dashboard"})
	case err != nil:
		out = append(out, check{statusWarn, "router.key", fmt.Sprintf("/v1/models failed: %v", err), ""})
	default:
		out = append(out, check{statusOK, "router.key", fmt.Sprintf("accepted (from %s); %d models listed", r.KeySource, len(models)), ""})
	}
	return out
}

// checkDashProxies asks the router dashboard whether it proxies tokenator
// (/tokens/) and agent-monitor (/monitor/): the one page depends on both.
// 404 is the router's own "not proxied" answer and names the flag; a 3xx
// means a front door (SSO) answered before the router and the probe cannot
// tell from here; anything else below 500 is wired.
func checkDashProxies(ctx context.Context, env doctorEnv, dashboard string) []check {
	if env.offline {
		return nil
	}
	base := strings.TrimRight(dashboard, "/")
	var out []check
	for _, p := range []struct{ area, path, flag string }{
		{"dash tokens", "/tokens/", "--dashboard-tokens-url (pitf exports PITF_TOKENS_URL)"},
		{"dash monitor", "/monitor/api/agents", "--dashboard-monitor-url (pitf exports PITF_MONITOR_URL)"},
	} {
		st, err := probe(ctx, env.http, base+p.path, "")
		switch {
		case err != nil:
			out = append(out, check{statusWarn, p.area, fmt.Sprintf("%s%s unreachable: %v", base, p.path, err), ""})
		case st == http.StatusNotFound:
			out = append(out, check{statusWarn, p.area, fmt.Sprintf("%s%s is not proxied (404)", base, p.path), "start the router with " + p.flag + " so the dashboard's tab works"})
		case st >= 300 && st < 400:
			out = append(out, check{statusInfo, p.area, fmt.Sprintf("%s%s answered %d from the front door (SSO); proxy state not visible from here", base, p.path, st), ""})
		case st >= 500:
			out = append(out, check{statusWarn, p.area, fmt.Sprintf("%s%s HTTP %d: the proxy is configured but its target is down", base, p.path, st), "start the tool (`pitf tokens serve` / `pitf monitor`) or fix the router's target URL"})
		default:
			out = append(out, check{statusOK, p.area, fmt.Sprintf("%s%s proxied (HTTP %d)", base, p.path, st), ""})
		}
	}
	return out
}

// checkToolURL is "does anything answer there": any HTTP status below 500
// counts (a 302 to SSO, a 401, a 404 on the probe path all mean up).
func checkToolURL(ctx context.Context, env doctorEnv, area, url, hint string) check {
	if env.offline {
		return check{statusInfo, area, url + " (not probed: --offline)", ""}
	}
	st, err := probe(ctx, env.http, url, "")
	switch {
	case err != nil:
		return check{statusWarn, area, fmt.Sprintf("%s unreachable: %v", url, err), hint}
	case st >= 500:
		return check{statusWarn, area, fmt.Sprintf("%s HTTP %d", url, st), hint}
	default:
		return check{statusOK, area, fmt.Sprintf("%s (HTTP %d)", url, st), ""}
	}
}

func probe(ctx context.Context, c *http.Client, url, bearer string) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, err
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := c.Do(req)
	if err != nil {
		var ue interface{ Unwrap() error }
		if errors.As(err, &ue) && ue.Unwrap() != nil {
			err = ue.Unwrap() // drop the "Get \"url\":" prefix; the line already names the URL
		}
		return 0, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	return resp.StatusCode, nil
}

func checkPython(r *config.Resolved, env doctorEnv) []check {
	var out []check
	root := r.Tools.SmithyDir
	if root == "" {
		root = config.DefaultSmithyDir()
	}
	if p, err := env.lookPath("uv"); err == nil {
		out = append(out, check{statusOK, "uv", p, ""})
	} else {
		out = append(out, check{statusWarn, "uv", "not on PATH; pitf " + strings.Join(pyToolNames(), " | ") + " need it", uvInstallHint})
	}
	if fi, err := os.Stat(root); err != nil || !fi.IsDir() {
		out = append(out, check{statusWarn, "smithy_dir", root + " does not exist", "mkdir it and clone the projects below, or set [tools].smithy_dir / PITF_SMITHY_DIR"})
	} else {
		out = append(out, check{statusOK, "smithy_dir", root, ""})
	}
	for _, t := range pyTools {
		if t.present(root) {
			out = append(out, check{statusOK, "pitf " + t.name, t.projectDir(root) + " (" + t.script + ")", ""})
		} else {
			out = append(out, check{statusWarn, "pitf " + t.name, "no checkout at " + t.projectDir(root), t.missingHint(root)})
		}
	}
	return out
}

func checkExternals(env doctorEnv) []check {
	var out []check
	for _, n := range listExternals(env.pathEnv) {
		p, _ := lookupExternal(n, env.pathEnv)
		switch {
		case n == "bench-py":
			out = append(out, check{statusWarn, "external", p + " is the retired bench-py shim", "rm it; llm-router-bench runs with `uv run --project <smithy_dir>/llm-router llm-router-bench`"})
		case env.root != nil && isBuiltin(env.root, n):
			out = append(out, check{statusWarn, "external", fmt.Sprintf("%s is shadowed by the built-in `pitf %s` and never runs", p, n), "rm it"})
		default:
			out = append(out, check{statusInfo, "external", fmt.Sprintf("pitf %s → %s", n, p), ""})
		}
	}
	return out
}

// Package config resolves pitf's one operator config: where the router is,
// how to authenticate to it, and any extra environment a profile wants
// every subcommand to see.
//
// Precedence, highest first:
//
//	explicit flag (--profile, --router-url)
//	PITF_* environment (PITF_PROFILE, PITF_ROUTER_URL, PITF_ROUTER_API_KEY)
//	per-tool environment already set (ROUTER_API_KEY) — only when no profile
//	    was chosen explicitly; an explicit profile means "use that router"
//	the selected profile's [profiles.<name>.router] and [profiles.<name>.env]
//	the file's top-level [router] and [env] defaults
package config

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"
)

// Router is the shared "where is the router and how do I talk to it" block,
// used both at the top level of the file and inside each profile.
type Router struct {
	URL       string `toml:"url"`
	APIKey    string `toml:"api_key"`     // literal; prefer api_key_cmd
	APIKeyCmd string `toml:"api_key_cmd"` // shell command whose stdout is the key
}

// Tools is where each tool's web UI lives, for cross-tool jumps
// (`pitf session`, `pitf model`). Defaults are the tools' own loopback
// listeners; the dashboard has no default because it is normally reached
// through a front proxy.
type Tools struct {
	MonitorURL   string `toml:"monitor_url"`
	TokensURL    string `toml:"tokens_url"`
	DashboardURL string `toml:"dashboard_url"`
	// SmithyDir holds the sibling checkouts the Python tools run from
	// (`pitf qual|forge|meta` → `uv run --project <SmithyDir>/<project>`).
	// Default ~/code/smithy; PITF_SMITHY_DIR overrides.
	SmithyDir string `toml:"smithy_dir"`
}

// Nous is the Forge notebook's daemon: where `pitf bench import` writes
// benchmark rows. Same shape as Router; usually only reachable from home.
type Nous struct {
	URL       string `toml:"url"`
	APIKey    string `toml:"api_key"`
	APIKeyCmd string `toml:"api_key_cmd"`
}

// Services is what `pitf services install` sets up, so re-running it with
// no flags reproduces the same agents. Command-line flags override the
// scalars and append to the lists.
type Services struct {
	ModelsYAML     string   `toml:"models_yaml"`
	RouterAddr     string   `toml:"router_addr"`
	DashboardAddr  string   `toml:"dashboard_addr"`
	RouterArgs     []string `toml:"router_args"`
	RouterEnvFiles []string `toml:"router_env_files"`
	IngestArgs     []string `toml:"ingest_args"`
	IngestEvery    string   `toml:"ingest_every"` // Go duration, e.g. "5m"
}

// merge lays a profile's services over the defaults: a set scalar wins, and
// a set list replaces (not extends) the default list, so a profile can say
// exactly what it runs.
func (s Services) merge(over Services) Services {
	str := func(a, b string) string {
		if b != "" {
			return b
		}
		return a
	}
	list := func(a, b []string) []string {
		if b != nil {
			return b
		}
		return a
	}
	return Services{
		ModelsYAML:     str(s.ModelsYAML, over.ModelsYAML),
		RouterAddr:     str(s.RouterAddr, over.RouterAddr),
		DashboardAddr:  str(s.DashboardAddr, over.DashboardAddr),
		RouterArgs:     list(s.RouterArgs, over.RouterArgs),
		RouterEnvFiles: list(s.RouterEnvFiles, over.RouterEnvFiles),
		IngestArgs:     list(s.IngestArgs, over.IngestArgs),
		IngestEvery:    str(s.IngestEvery, over.IngestEvery),
	}
}

// Profile overrides the top-level defaults field by field.
type Profile struct {
	Router   Router            `toml:"router"`
	Nous     Nous              `toml:"nous"`
	Tools    Tools             `toml:"tools"`
	Services Services          `toml:"services"`
	Env      map[string]string `toml:"env"`
}

// File is the on-disk shape of ~/.config/pitf/config.toml.
type File struct {
	DefaultProfile string             `toml:"default_profile"`
	Router         Router             `toml:"router"`
	Nous           Nous               `toml:"nous"`
	Tools          Tools              `toml:"tools"`
	Services       Services           `toml:"services"`
	Env            map[string]string  `toml:"env"`
	Profiles       map[string]Profile `toml:"profiles"`
}

// Built-in tool defaults.
const (
	DefaultMonitorURL = "http://127.0.0.1:8070"
	DefaultTokensURL  = "http://127.0.0.1:8990"
)

// Options are the caller-supplied inputs to Resolve: the explicit flags and
// a Getenv hook (tests inject one; nil means os.Getenv).
type Options struct {
	Profile   string
	RouterURL string
	Getenv    func(string) string
	// Run executes an api_key_cmd and returns its stdout. nil means sh -c.
	Run func(cmd string) (string, error)
}

// Resolved is what a subcommand actually gets.
type Resolved struct {
	Path      string // config file consulted (may not exist)
	FileFound bool

	Profile         string // "" when none selected
	ProfileSource   string // "--profile", "PITF_PROFILE", "default_profile", or ""
	ProfileExplicit bool   // chosen by flag or PITF_PROFILE

	RouterURL       string
	RouterURLSource string

	KeySource  string // where the key comes from, for `config show`
	keyLiteral string
	keyCmd     string
	keyCache   string
	keyDone    bool
	run        func(string) (string, error)

	// Nous daemon, resolved like the router: PITF_NOUS_URL > ambient
	// NOUS_DAEMON_URL (implicit profile only) > profile > defaults; key from
	// PITF_NOUS_API_KEY > ambient NOUS_API_KEY > profile > defaults.
	NousURL       string
	NousURLSource string
	NousKeySource string
	nousKeyLit    string
	nousKeyCmd    string
	nousKeyCache  string
	nousKeyDone   bool

	// Tools is the merged [tools] + [profiles.X.tools] (profile wins per
	// field), with built-in defaults for monitor and tokens.
	Tools Tools

	// Services is the merged [services] + [profiles.X.services].
	Services Services

	// Env is the merged [env] + [profiles.X.env] tables (profile wins).
	Env map[string]string
	// Profiles lists the names defined in the file, sorted.
	Profiles []string

	getenv func(string) string
}

// Env var names pitf exports to every subcommand.
const (
	EnvProfile   = "PITF_PROFILE"
	EnvRouterURL = "PITF_ROUTER_URL"
	EnvAPIKey    = "PITF_ROUTER_API_KEY"
	// EnvToolAPIKey is what the llm-router Python tools (qualeval, bench, …)
	// read today; pitf sets it so they need no change.
	EnvToolAPIKey = "ROUTER_API_KEY"
	// The cross-link variables the mounted tools read, fed from [tools] so
	// `pitf monitor` and `pitf tokens serve` link to each other with no
	// flags: agent-monitor's board links to tokenator, tokenator's session
	// pages link back to the board.
	EnvMonitorTokensURL = "AGENT_MONITOR_TOKENS_URL"
	EnvTokenatorMonitor = "TOKENATOR_MONITOR_URL"
	EnvPitfNousURL      = "PITF_NOUS_URL"
	EnvPitfNousAPIKey   = "PITF_NOUS_API_KEY"
	EnvNousURL          = "NOUS_DAEMON_URL" // what nous-mcp, forge and meta read
	EnvNousAPIKey       = "NOUS_API_KEY"
	EnvPitfMonitorURL   = "PITF_MONITOR_URL"
	EnvPitfTokensURL    = "PITF_TOKENS_URL"
	EnvPitfDashboardURL = "PITF_DASHBOARD_URL"
	EnvPitfSmithyDir    = "PITF_SMITHY_DIR"
)

// DefaultSmithyDir is ~/code/smithy: where the sibling checkouts live.
func DefaultSmithyDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join("code", "smithy")
	}
	return filepath.Join(home, "code", "smithy")
}

// DefaultPath is $XDG_CONFIG_HOME/pitf/config.toml, falling back to
// ~/.config/pitf/config.toml. PITF_CONFIG overrides the whole path.
func DefaultPath(getenv func(string) string) string {
	if getenv == nil {
		getenv = os.Getenv
	}
	if p := getenv("PITF_CONFIG"); p != "" {
		return p
	}
	base := getenv("XDG_CONFIG_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			home = "."
		}
		base = filepath.Join(home, ".config")
	}
	return filepath.Join(base, "pitf", "config.toml")
}

// Load parses the file at path. A missing file is not an error: it yields
// an empty File and found=false.
func Load(path string) (f File, found bool, err error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return File{}, false, nil
	}
	if err != nil {
		return File{}, false, err
	}
	md, err := toml.Decode(string(data), &f)
	if err != nil {
		return File{}, true, fmt.Errorf("%s: %w", path, err)
	}
	if undecoded := md.Undecoded(); len(undecoded) > 0 {
		keys := make([]string, 0, len(undecoded))
		for _, k := range undecoded {
			keys = append(keys, k.String())
		}
		return File{}, true, fmt.Errorf("%s: unknown keys: %s", path, strings.Join(keys, ", "))
	}
	return f, true, nil
}

// Resolve loads the default config path and applies the precedence rules.
func Resolve(opts Options) (*Resolved, error) {
	getenv := opts.Getenv
	if getenv == nil {
		getenv = os.Getenv
	}
	path := DefaultPath(getenv)
	f, found, err := Load(path)
	if err != nil {
		return nil, err
	}
	return resolve(f, path, found, opts, getenv)
}

func resolve(f File, path string, found bool, opts Options, getenv func(string) string) (*Resolved, error) {
	r := &Resolved{Path: path, FileFound: found, getenv: getenv, run: opts.Run}
	if r.run == nil {
		r.run = runShell
	}
	for name := range f.Profiles {
		r.Profiles = append(r.Profiles, name)
	}
	sort.Strings(r.Profiles)

	// 1. Which profile.
	switch {
	case opts.Profile != "":
		r.Profile, r.ProfileSource, r.ProfileExplicit = opts.Profile, "--profile", true
	case getenv(EnvProfile) != "":
		r.Profile, r.ProfileSource, r.ProfileExplicit = getenv(EnvProfile), EnvProfile, true
	case f.DefaultProfile != "":
		r.Profile, r.ProfileSource = f.DefaultProfile, "default_profile"
	}
	var prof Profile
	if r.Profile != "" {
		p, ok := f.Profiles[r.Profile]
		if !ok {
			return nil, fmt.Errorf("profile %q not defined in %s (have: %s)", r.Profile, path, strings.Join(r.Profiles, ", "))
		}
		prof = p
	}

	// 2. Router URL.
	switch {
	case opts.RouterURL != "":
		r.RouterURL, r.RouterURLSource = opts.RouterURL, "--router-url"
	case getenv(EnvRouterURL) != "":
		r.RouterURL, r.RouterURLSource = getenv(EnvRouterURL), EnvRouterURL
	case prof.Router.URL != "":
		r.RouterURL, r.RouterURLSource = prof.Router.URL, "profile "+r.Profile
	case f.Router.URL != "":
		r.RouterURL, r.RouterURLSource = f.Router.URL, "[router] defaults"
	}

	// 3. API key. Secrets never come from a flag.
	switch {
	case getenv(EnvAPIKey) != "":
		r.keyLiteral, r.KeySource = getenv(EnvAPIKey), EnvAPIKey
	case !r.ProfileExplicit && getenv(EnvToolAPIKey) != "":
		r.keyLiteral, r.KeySource = getenv(EnvToolAPIKey), EnvToolAPIKey+" (already set)"
	case prof.Router.APIKey != "":
		r.keyLiteral, r.KeySource = prof.Router.APIKey, "profile "+r.Profile+" (literal)"
	case prof.Router.APIKeyCmd != "":
		r.keyCmd, r.KeySource = prof.Router.APIKeyCmd, "profile "+r.Profile+" (command)"
	case f.Router.APIKey != "":
		r.keyLiteral, r.KeySource = f.Router.APIKey, "[router] defaults (literal)"
	case f.Router.APIKeyCmd != "":
		r.keyCmd, r.KeySource = f.Router.APIKeyCmd, "[router] defaults (command)"
	default:
		r.KeySource = "none"
	}

	// 4. Tool UIs: built-in default < [tools] < profile < PITF_*_URL env.
	pick := func(env, prof, top, def string) string {
		for _, v := range []string{getenv(env), prof, top, def} {
			if v != "" {
				return v
			}
		}
		return ""
	}
	r.Tools = Tools{
		MonitorURL:   pick("PITF_MONITOR_URL", prof.Tools.MonitorURL, f.Tools.MonitorURL, DefaultMonitorURL),
		TokensURL:    pick("PITF_TOKENS_URL", prof.Tools.TokensURL, f.Tools.TokensURL, DefaultTokensURL),
		DashboardURL: pick("PITF_DASHBOARD_URL", prof.Tools.DashboardURL, f.Tools.DashboardURL, ""),
		SmithyDir:    ExpandHome(pick(EnvPitfSmithyDir, prof.Tools.SmithyDir, f.Tools.SmithyDir, DefaultSmithyDir())),
	}

	// 5. Nous daemon (optional).
	switch {
	case getenv(EnvPitfNousURL) != "":
		r.NousURL, r.NousURLSource = getenv(EnvPitfNousURL), EnvPitfNousURL
	case !r.ProfileExplicit && getenv(EnvNousURL) != "":
		r.NousURL, r.NousURLSource = getenv(EnvNousURL), EnvNousURL+" (already set)"
	case prof.Nous.URL != "":
		r.NousURL, r.NousURLSource = prof.Nous.URL, "profile "+r.Profile
	case f.Nous.URL != "":
		r.NousURL, r.NousURLSource = f.Nous.URL, "[nous] defaults"
	}
	switch {
	case getenv(EnvPitfNousAPIKey) != "":
		r.nousKeyLit, r.NousKeySource = getenv(EnvPitfNousAPIKey), EnvPitfNousAPIKey
	case !r.ProfileExplicit && getenv(EnvNousAPIKey) != "":
		r.nousKeyLit, r.NousKeySource = getenv(EnvNousAPIKey), EnvNousAPIKey+" (already set)"
	case prof.Nous.APIKey != "":
		r.nousKeyLit, r.NousKeySource = prof.Nous.APIKey, "profile "+r.Profile+" (literal)"
	case prof.Nous.APIKeyCmd != "":
		r.nousKeyCmd, r.NousKeySource = prof.Nous.APIKeyCmd, "profile "+r.Profile+" (command)"
	case f.Nous.APIKey != "":
		r.nousKeyLit, r.NousKeySource = f.Nous.APIKey, "[nous] defaults (literal)"
	case f.Nous.APIKeyCmd != "":
		r.nousKeyCmd, r.NousKeySource = f.Nous.APIKeyCmd, "[nous] defaults (command)"
	default:
		r.NousKeySource = "none"
	}

	r.Services = f.Services.merge(prof.Services)

	// 6. Extra env: defaults, then profile on top.
	r.Env = map[string]string{}
	for k, v := range f.Env {
		r.Env[k] = v
	}
	for k, v := range prof.Env {
		r.Env[k] = v
	}
	return r, nil
}

// HasKey reports whether some key source exists, without resolving it.
func (r *Resolved) HasKey() bool { return r.keyLiteral != "" || r.keyCmd != "" }

// KeyCmd returns the configured command, if the key comes from one.
func (r *Resolved) KeyCmd() string { return r.keyCmd }

// APIKey returns the router bearer, running api_key_cmd at most once and
// only when asked. Returns "" when no source is configured.
func (r *Resolved) APIKey() (string, error) {
	if r.keyLiteral != "" {
		return r.keyLiteral, nil
	}
	if r.keyCmd == "" {
		return "", nil
	}
	if r.keyDone {
		return r.keyCache, nil
	}
	out, err := r.run(r.keyCmd)
	if err != nil {
		return "", fmt.Errorf("api_key_cmd %q: %w", r.keyCmd, err)
	}
	r.keyCache, r.keyDone = strings.TrimSpace(out), true
	if r.keyCache == "" {
		return "", fmt.Errorf("api_key_cmd %q printed nothing", r.keyCmd)
	}
	return r.keyCache, nil
}

// HasNous reports whether a Nous daemon URL is configured.
func (r *Resolved) HasNous() bool { return r.NousURL != "" }

// NousAPIKey resolves the Nous key lazily, running its command at most once.
func (r *Resolved) NousAPIKey() (string, error) {
	if r.nousKeyLit != "" {
		return r.nousKeyLit, nil
	}
	if r.nousKeyCmd == "" {
		return "", nil
	}
	if r.nousKeyDone {
		return r.nousKeyCache, nil
	}
	out, err := r.run(r.nousKeyCmd)
	if err != nil {
		return "", fmt.Errorf("nous api_key_cmd %q: %w", r.nousKeyCmd, err)
	}
	r.nousKeyCache, r.nousKeyDone = strings.TrimSpace(out), true
	if r.nousKeyCache == "" {
		return "", fmt.Errorf("nous api_key_cmd %q printed nothing", r.nousKeyCmd)
	}
	return r.nousKeyCache, nil
}

// Environment returns the variables pitf exports to a subcommand, in a
// stable order. The key command runs only if a key is needed, i.e. there is
// a source and nothing already satisfies it.
func (r *Resolved) Environment() ([]string, error) {
	set := map[string]string{}
	if r.Profile != "" {
		set[EnvProfile] = r.Profile
	}
	if r.RouterURL != "" {
		set[EnvRouterURL] = r.RouterURL
	}
	if r.HasKey() {
		key, err := r.APIKey()
		if err != nil {
			return nil, err
		}
		set[EnvAPIKey] = key
		set[EnvToolAPIKey] = key
	}
	// Nous, only when configured: forge and meta read these names.
	if r.HasNous() {
		if r.ProfileExplicit || r.getenv(EnvNousURL) == "" {
			set[EnvNousURL] = r.NousURL
		}
		if r.nousKeyLit != "" || r.nousKeyCmd != "" {
			if r.ProfileExplicit || r.getenv(EnvNousAPIKey) == "" {
				key, err := r.NousAPIKey()
				if err != nil {
					return nil, err
				}
				set[EnvNousAPIKey] = key
			}
		}
	}

	// Tool cross-links and the resolved tool URLs, under the same
	// no-clobber rule as the user tables.
	tools := map[string]string{
		EnvMonitorTokensURL: r.Tools.TokensURL,
		EnvTokenatorMonitor: r.Tools.MonitorURL,
		EnvPitfMonitorURL:   r.Tools.MonitorURL,
		EnvPitfTokensURL:    r.Tools.TokensURL,
		EnvPitfDashboardURL: r.Tools.DashboardURL,
		EnvPitfSmithyDir:    r.Tools.SmithyDir,
	}
	for k, v := range tools {
		if v == "" || (!r.ProfileExplicit && r.getenv(k) != "") {
			continue
		}
		set[k] = v
	}
	for k, v := range r.Env {
		// User tables never clobber something the operator already
		// exported by hand unless they chose the profile explicitly.
		if !r.ProfileExplicit && r.getenv(k) != "" {
			continue
		}
		set[k] = v
	}
	keys := make([]string, 0, len(set))
	for k := range set {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, k+"="+set[k])
	}
	return out, nil
}

// Apply sets Environment() into the process so an exec'd subcommand
// inherits it.
func (r *Resolved) Apply() error {
	env, err := r.Environment()
	if err != nil {
		return err
	}
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		if err := os.Setenv(k, v); err != nil {
			return err
		}
	}
	return nil
}

func runShell(cmd string) (string, error) {
	c := exec.Command("sh", "-c", cmd)
	c.Stdin = os.Stdin // ho may need a tty for an unlock prompt
	c.Stderr = os.Stderr
	out, err := c.Output()
	return string(out), err
}

// Example is a starter file written by `pitf config init`.
const Example = `# pitf config — one place to say where the router is and how to reach it.
# Precedence: flag > PITF_* env > (ROUTER_API_KEY already set, unless a
# profile was chosen explicitly) > profile > these defaults.

default_profile = "home"

[router]
url = "https://llm.bcc.sh"
api_key_cmd = "ho secret get llm-router/api-key"

# Nous daemon (Forge notebook) for pitf bench import. Optional; leave it
# out on a box that cannot reach it. A PAT file or ho secret both work.
# [nous]
# url = "https://app.nous.page"
# api_key_cmd = "cat ~/.config/nous/euclid-pat"

# Where the tools' web UIs live, for pitf session / pitf model jumps.
# monitor/tokens default to the tools' loopback ports; the dashboard has no
# default (it usually sits behind a front proxy).
[tools]
# monitor_url = "http://127.0.0.1:8070"
# tokens_url = "http://127.0.0.1:8990"
# dashboard_url = "https://llm.example/dashboard"
# Where the Python tools' checkouts live (pitf qual / forge / meta run
# "uv run --project" there). PITF_SMITHY_DIR overrides.
# smithy_dir = "~/code/smithy"

# Extra environment every subcommand should see (profile tables override).
[env]

[profiles.home]
# inherits [router]

[profiles.work.router]
url = "https://router.example.corp"
# api_key = "literal-if-you-must"
# api_key_cmd = "op read op://work/router/credential"

[profiles.work.env]
# FORGE-style per-agent vars, or anything else, go here:
# CODE_REVIEWER_OPENAI_BASE_URL = "https://router.example.corp/v1"

# What pitf services install runs on a laptop (macOS launchd). Re-running
# install with no flags reproduces exactly this; flags override or append.
# [profiles.work.services]
# models_yaml = "~/.config/llm-router/models.yaml"
# router_env_files = ["~/.config/llm-router/router.env"]
# router_args = ["-log-format=text", "-wellknown-provider-name=Work Router"]
# ingest_every = "5m"
`

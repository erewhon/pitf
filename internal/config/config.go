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

// Profile overrides the top-level defaults field by field.
type Profile struct {
	Router Router            `toml:"router"`
	Env    map[string]string `toml:"env"`
}

// File is the on-disk shape of ~/.config/pitf/config.toml.
type File struct {
	DefaultProfile string             `toml:"default_profile"`
	Router         Router             `toml:"router"`
	Env            map[string]string  `toml:"env"`
	Profiles       map[string]Profile `toml:"profiles"`
}

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
)

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

	// 4. Extra env: defaults, then profile on top.
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
`

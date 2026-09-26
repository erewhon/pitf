package cli

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/erewhon/pitf/internal/config"
	"github.com/erewhon/pitf/internal/services"
)

// hostManager wires the real launchctl and ~/Library paths. Tests replace it.
var hostManager = func(out io.Writer) (services.Manager, error) {
	l, err := services.Host()
	if err != nil {
		return services.Manager{}, err
	}
	dirs, err := services.UserDirs()
	if err != nil {
		return services.Manager{}, err
	}
	return services.Manager{Dirs: dirs, L: l, Out: out, Probe: services.HTTPProbe}, nil
}

// readPlist is services.ReadPlist, swappable in tests.
var readPlist services.ReadFunc = services.ReadPlist

type installFlags struct {
	modelsYAML    string
	routerAddr    string
	dashboardAddr string
	routerArgs    []string
	envFiles      []string
	ingestArgs    []string
	ingestEvery   time.Duration
	replaceLegacy bool
	dryRun        bool
}

func newServicesCmd(gf *globalFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "services",
		Short: "Run the router (with its dashboard), tokenator and ingest as launchd agents (macOS)",
		Long: "Installs per-user launchd agents for the laptop stack:\n\n" +
			"  router     pitf router serve --dashboard   (127.0.0.1:4010, dashboard :4011 — the one page;\n" +
			"             it proxies tokenator at /tokens/ and agent-monitor at /monitor/)\n" +
			"  tokens     pitf tokens serve               (127.0.0.1:8990)\n" +
			"  ingest     pitf tokens ingest, every 5 minutes\n\n" +
			"Each agent runs pitf itself, so it gets the same config, keys and\n" +
			"cross-link environment as an interactive run. They start at login and\n" +
			"restart if they exit. agent-monitor is not one of them: it is a TUI,\n" +
			"so `pitf up` makes sure the agents are loaded and then runs it.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}
	cmd.AddCommand(newServicesInstallCmd(gf), newServicesUninstallCmd(), newServicesStatusCmd(),
		newServicesRestartCmd(), newServicesStopCmd(), newServicesStartCmd())
	return cmd
}

func newServicesInstallCmd(gf *globalFlags) *cobra.Command {
	f := &installFlags{}
	cmd := &cobra.Command{
		Use:   "install",
		Short: "Write and load the launchd agents (idempotent; re-run after changing flags)",
		Long: "Writes ~/Library/LaunchAgents/" + services.LabelPrefix + "*.plist and loads them.\n" +
			"Re-running rewrites only what changed and reloads only those agents.\n\n" +
			"An existing launchd agent for the standalone llm-router is adopted: its\n" +
			"flags (minus -addr and the dashboard flags, which pitf now owns) and its\n" +
			"EnvironmentVariables (upstream keys such as AWS_BEARER_TOKEN_BEDROCK)\n" +
			"move into the pitf router agent, it is stopped, and its plist is moved\n" +
			"to ~/Library/Application Support/pitf/replaced/ (not deleted).\n\n" +
			"The router serves /.well-known/opencode by default (provider id\n" +
			"\"llm\", base URL from the router address); override or disable with\n" +
			"--router-arg=-wellknown-provider-id=<id> (empty turns it off).\n\n" +
			"Settings come from [services] / [profiles.<name>.services] in the pitf\n" +
			"config; flags override its scalars and append to its lists, so a bare\n" +
			"install always reproduces the config.\n\n" +
			"Upstream keys belong in --router-env-file (default\n" +
			"~/.config/llm-router/router.env when present): the router agent loads it\n" +
			"at each start via pitf --env-file, so they never land in a plist.\n\n" +
			"The config's profile is baked in only when chosen explicitly (--profile\n" +
			"or PITF_PROFILE); otherwise the agents follow default_profile.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runInstall(cmd, gf, f)
		},
	}
	fl := cmd.Flags()
	fl.StringVar(&f.modelsYAML, "models-yaml", "", "router models.yaml (default: the adopted agent's, else ~/.config/llm-router/models.yaml)")
	fl.StringVar(&f.routerAddr, "router-addr", services.DefaultRouterAddr, "router listen address")
	fl.StringVar(&f.dashboardAddr, "dashboard-addr", services.DefaultDashboardAddr, "router dashboard listen address (keep it loopback: no auth)")
	fl.StringArrayVar(&f.routerArgs, "router-arg", nil, "extra flag for `pitf router serve` (repeatable), e.g. --router-arg=-log-format=text")
	fl.StringArrayVar(&f.envFiles, "router-env-file", nil, "KEY=VALUE file the router agent loads at each start, for upstream keys (repeatable; default ~/.config/llm-router/router.env when it exists)")
	fl.StringArrayVar(&f.ingestArgs, "ingest-arg", nil, "extra flag for `pitf tokens ingest` (repeatable), e.g. --ingest-arg=-regime=metered")
	fl.DurationVar(&f.ingestEvery, "ingest-every", services.DefaultIngestEvery, "how often tokenator ingests new session data")
	fl.BoolVar(&f.replaceLegacy, "replace-legacy", false, "stop and move aside a legacy router agent even when its flags cannot be adopted (shell wrapper)")
	fl.BoolVar(&f.dryRun, "dry-run", false, "print the plists instead of writing them")
	return cmd
}

func runInstall(cmd *cobra.Command, gf *globalFlags, f *installFlags) error {
	out := cmd.OutOrStdout()
	r, err := config.Resolve(gf.options())
	if err != nil {
		return err
	}
	if err := applyServicesConfig(cmd, r, f); err != nil {
		return err
	}
	if err := services.CheckLoopback(f.dashboardAddr); err != nil {
		return fmt.Errorf("dashboard address: %w", err)
	}
	m, err := hostManager(out)
	if err != nil && !f.dryRun {
		return err
	}
	if m.Dirs.Agents == "" { // dry run off macOS: still show the plists
		if m.Dirs, err = services.UserDirs(); err != nil {
			return err
		}
	}

	opts := services.Options{
		Pitf:          pitfPath(),
		ModelsYAML:    f.modelsYAML,
		RouterAddr:    f.routerAddr,
		DashboardAddr: f.dashboardAddr,
		IngestEvery:   f.ingestEvery,
		IngestArgs:    f.ingestArgs,
	}
	if r.ProfileExplicit {
		opts.Profile = r.Profile
	}

	// Adopt the pre-pitf router agent, if there is exactly one.
	legacy, err := services.FindLegacy(m.Dirs.Agents, readPlist)
	if err != nil {
		return err
	}
	var retire *services.Agent
	switch {
	case len(legacy) > 1:
		var paths []string
		for _, a := range legacy {
			paths = append(paths, a.Path)
		}
		return fmt.Errorf("more than one launchd agent runs llm-router; unload all but one first:\n  %s", strings.Join(paths, "\n  "))
	case len(legacy) == 1:
		a := legacy[0]
		ad, err := services.Adopt(a)
		switch {
		case err != nil && !f.replaceLegacy:
			return err
		case err != nil:
			fmt.Fprintf(out, "legacy    %s: not adopting its flags or environment (--replace-legacy)\n", a.Path)
		default:
			fmt.Fprintf(out, "legacy    adopting %s\n", a.Path)
			if opts.ModelsYAML == "" {
				opts.ModelsYAML = ad.ModelsYAML
			}
			opts.RouterArgs = ad.Args
			opts.RouterEnv = ad.Env
			if len(ad.Env) > 0 {
				fmt.Fprintf(out, "          environment: %s\n", strings.Join(sortedEnvKeys(ad.Env), ", "))
			}
			if len(ad.Args) > 0 {
				fmt.Fprintf(out, "          flags: %s\n", strings.Join(ad.Args, " "))
			}
			if len(ad.Dropped) > 0 {
				fmt.Fprintf(out, "          replaced by pitf's: %s\n", strings.Join(ad.Dropped, " "))
			}
		}
		retire = &a
	}
	// Order is precedence: the router's flag parser keeps the last value, so
	// the well-known defaults come first and anything adopted, configured or
	// typed after them wins (`-wellknown-provider-id=` turns it off).
	opts.RouterArgs = append(wellKnownDefaults(f.routerAddr), append(opts.RouterArgs, f.routerArgs...)...)
	if opts.RouterEnvFiles, err = routerEnvFiles(out, f.envFiles); err != nil {
		return err
	}
	if opts.ModelsYAML == "" {
		opts.ModelsYAML = defaultModelsYAML()
	}
	if _, err := os.Stat(opts.ModelsYAML); err != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "warning: %s: %v (the router agent will restart until it exists)\n", opts.ModelsYAML, err)
	}
	// `pitf up`, `pitf session` and `pitf model` all want to know where the
	// router dashboard is; on a laptop that is the router agent's own.
	if r.Tools.DashboardURL == "" {
		fmt.Fprintf(out, "note      no [tools] dashboard_url in %s; `pitf up` opens http://%s/.\n"+
			"          Add dashboard_url to the profile so `pitf session` / `pitf model` link there too.\n", r.Path, f.dashboardAddr)
	}

	specs := services.Plan(opts)
	if f.dryRun {
		for _, s := range specs {
			fmt.Fprintf(out, "\n# %s\n%s", m.Dirs.PlistPath(s.Name), services.Plist(s, m.Dirs.Logs))
		}
		return nil
	}
	if err := m.Install(specs, retire); err != nil {
		return err
	}
	fmt.Fprintf(out, "\nlogs: %s\nNext: `pitf up` starts agent-monitor and opens the router dashboard at %s\n",
		m.Dirs.Logs, dashboardHome(r, f.dashboardAddr))
	return nil
}

func newServicesUninstallCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "uninstall",
		Short: "Stop and remove the pitf agents (logs and any replaced legacy plist stay)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			m, err := hostManager(cmd.OutOrStdout())
			if err != nil {
				return err
			}
			return m.Uninstall()
		},
	}
}

func newServicesStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show each agent's launchd state and whether it answers",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			m, err := hostManager(cmd.OutOrStdout())
			if err != nil {
				return err
			}
			return m.Status(cmd.Context(), installedSpecs(m))
		},
	}
}

func newServicesRestartCmd() *cobra.Command {
	return &cobra.Command{
		Use:       "restart [service…]",
		Short:     "Restart agents (all installed when none named), e.g. after editing models.yaml",
		ValidArgs: services.Names,
		Args:      cobra.OnlyValidArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			m, err := hostManager(cmd.OutOrStdout())
			if err != nil {
				return err
			}
			return m.Restart(args)
		},
	}
}

func newServicesStopCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "stop",
		Short: "Stop every agent until `pitf up` / `pitf services start` (they stay installed)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			m, err := hostManager(cmd.OutOrStdout())
			if err != nil {
				return err
			}
			return m.Stop()
		},
	}
}

func newServicesStartCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "start",
		Short: "Load any installed agent that is stopped",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			m, err := hostManager(cmd.OutOrStdout())
			if err != nil {
				return err
			}
			return m.Up()
		},
	}
}

// applyServicesConfig fills install's options from [services] in the
// config: a flag typed on the command line overrides a scalar and appends to
// a list, so a bare `pitf services install` always reproduces the config.
func applyServicesConfig(cmd *cobra.Command, r *config.Resolved, f *installFlags) error {
	sv := r.Services
	fl := cmd.Flags()
	scalar := func(name string, dst *string, cfg string) {
		if !fl.Changed(name) && cfg != "" {
			*dst = config.ExpandHome(cfg)
		}
	}
	scalar("models-yaml", &f.modelsYAML, sv.ModelsYAML)
	scalar("router-addr", &f.routerAddr, sv.RouterAddr)
	scalar("dashboard-addr", &f.dashboardAddr, sv.DashboardAddr)
	if !fl.Changed("ingest-every") && sv.IngestEvery != "" {
		d, err := time.ParseDuration(sv.IngestEvery)
		if err != nil {
			return fmt.Errorf("%s: services.ingest_every: %w", r.Path, err)
		}
		f.ingestEvery = d
	}
	f.routerArgs = append(append([]string(nil), sv.RouterArgs...), f.routerArgs...)
	f.envFiles = append(append([]string(nil), sv.RouterEnvFiles...), f.envFiles...)
	f.ingestArgs = append(append([]string(nil), sv.IngestArgs...), f.ingestArgs...)
	if sv.ModelsYAML != "" || sv.RouterAddr != "" || sv.DashboardAddr != "" || sv.IngestEvery != "" ||
		sv.RouterArgs != nil || sv.RouterEnvFiles != nil || sv.IngestArgs != nil {
		where := "[services]"
		if r.Profile != "" {
			where = "[services] / [profiles." + r.Profile + ".services]"
		}
		fmt.Fprintf(cmd.OutOrStdout(), "settings  %s in %s\n", where, r.Path)
	}
	return nil
}

// wellKnownDefaults turns on the router's /.well-known/opencode for the
// laptop stack: OpenCode there is pointed at this router, and an endpoint
// that 404s makes OpenCode fail at startup. The base URL is the loopback
// address the agent listens on (127.0.0.1, not localhost, which a client
// may resolve to ::1 first).
func wellKnownDefaults(routerAddr string) []string {
	host, port, err := net.SplitHostPort(routerAddr)
	if err != nil {
		return nil
	}
	if host == "" || host == "0.0.0.0" || host == "::" || host == "localhost" {
		host = "127.0.0.1"
	}
	return []string{
		"-wellknown-provider-id=" + services.DefaultWellKnownProvider,
		"-wellknown-base-url=http://" + net.JoinHostPort(host, port) + "/v1",
	}
}

// routerEnvFiles resolves --router-env-file to absolute paths (launchd
// expands nothing) and checks each parses now, not at the agent's first
// start. With none given, the conventional router.env is picked up.
func routerEnvFiles(out io.Writer, given []string) ([]string, error) {
	if len(given) == 0 {
		def := config.ExpandHome(defaultRouterEnvFile)
		if !fileExists(def) {
			return nil, nil
		}
		given = []string{def}
	}
	var paths []string
	for _, p := range given {
		abs, err := filepath.Abs(config.ExpandHome(p))
		if err != nil {
			return nil, err
		}
		vars, err := config.ReadEnvFile(abs)
		if err != nil {
			return nil, fmt.Errorf("--router-env-file: %w", err)
		}
		names := make([]string, 0, len(vars))
		for _, v := range vars {
			names = append(names, v.Key)
		}
		fmt.Fprintf(out, "env file  %s (%s)\n", abs, strings.Join(names, ", "))
		if info, err := os.Stat(abs); err == nil && info.Mode().Perm()&0o077 != 0 {
			fmt.Fprintf(out, "          warning: %s is readable by others (chmod 600 it)\n", abs)
		}
		paths = append(paths, abs)
	}
	return paths, nil
}

const defaultRouterEnvFile = "~/.config/llm-router/router.env"

// installedSpecs rebuilds the specs for status probes, taking the router's
// listen address from its installed plist so a custom --router-addr is
// probed where it actually listens.
func installedSpecs(m services.Manager) []services.Spec {
	return services.Plan(installedOptions(m))
}

// installedOptions reads the addresses back out of the installed router
// agent (defaults when there is none).
func installedOptions(m services.Manager) services.Options {
	opts := services.Options{RouterAddr: services.DefaultRouterAddr, DashboardAddr: services.DefaultDashboardAddr}
	if a, err := readPlist(m.Dirs.PlistPath("router")); err == nil {
		for i, arg := range a.Args {
			if i+1 >= len(a.Args) {
				break
			}
			switch arg {
			case "-addr":
				opts.RouterAddr = a.Args[i+1]
			case "-dashboard-addr":
				opts.DashboardAddr = a.Args[i+1]
			}
		}
	}
	return opts
}

// pitfPath is the binary the agents run. Homebrew's Cellar path changes on
// every upgrade, so prefer the stable bin/ symlink in front of it.
func pitfPath() string {
	if p, err := exec.LookPath("pitf"); err == nil {
		if abs, err := filepath.Abs(p); err == nil && !strings.Contains(abs, "/Cellar/") {
			return abs
		}
	}
	exe, err := os.Executable()
	if err != nil {
		return "pitf"
	}
	if prefix, _, ok := strings.Cut(exe, "/Cellar/"); ok {
		if link := filepath.Join(prefix, "bin", "pitf"); fileExists(link) {
			return link
		}
	}
	return exe
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func defaultModelsYAML() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "models.yaml"
	}
	return filepath.Join(home, ".config", "llm-router", "models.yaml")
}

func sortedEnvKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sortStrings(keys)
	return keys
}

var errNoMonitor = errors.New("agent-monitor mount missing from this build")

package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/erewhon/pitf/internal/config"
	"github.com/erewhon/pitf/internal/dashboard"
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
	return services.Manager{Dirs: dirs, L: l, Out: out, Probe: dashboard.HTTPProbe}, nil
}

// readPlist is services.ReadPlist, swappable in tests.
var readPlist services.ReadFunc = services.ReadPlist

type installFlags struct {
	modelsYAML    string
	routerAddr    string
	dashboardAddr string
	routerArgs    []string
	ingestArgs    []string
	ingestEvery   time.Duration
	replaceLegacy bool
	dryRun        bool
}

func newServicesCmd(gf *globalFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "services",
		Short: "Run the router, tokenator, the dashboard and ingest as launchd agents (macOS)",
		Long: "Installs per-user launchd agents for the laptop stack:\n\n" +
			"  router     pitf router serve --dashboard   (127.0.0.1:4010, dashboard :4011)\n" +
			"  tokens     pitf tokens serve               (127.0.0.1:8990)\n" +
			"  dashboard  pitf dashboard                  (127.0.0.1:8960)\n" +
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
	fl.StringArrayVar(&f.ingestArgs, "ingest-arg", nil, "extra flag for `pitf tokens ingest` (repeatable), e.g. --ingest-arg=-regime=metered")
	fl.DurationVar(&f.ingestEvery, "ingest-every", services.DefaultIngestEvery, "how often tokenator ingests new session data")
	fl.BoolVar(&f.replaceLegacy, "replace-legacy", false, "stop and move aside a legacy router agent even when its flags cannot be adopted (shell wrapper)")
	fl.BoolVar(&f.dryRun, "dry-run", false, "print the plists instead of writing them")
	return cmd
}

func runInstall(cmd *cobra.Command, gf *globalFlags, f *installFlags) error {
	out := cmd.OutOrStdout()
	if err := dashboard.CheckLoopback(f.dashboardAddr); err != nil {
		return fmt.Errorf("--dashboard-addr: %w", err)
	}
	r, err := config.Resolve(gf.options())
	if err != nil {
		return err
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
	opts.RouterArgs = append(opts.RouterArgs, f.routerArgs...)
	if opts.ModelsYAML == "" {
		opts.ModelsYAML = defaultModelsYAML()
	}
	if _, err := os.Stat(opts.ModelsYAML); err != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "warning: %s: %v (the router agent will restart until it exists)\n", opts.ModelsYAML, err)
	}
	// The pitf dashboard frames the router dashboard only when it knows
	// where it is; with no [tools] dashboard_url, point it at ours.
	if r.Tools.DashboardURL == "" {
		opts.DashboardURL = "http://" + f.dashboardAddr
		fmt.Fprintf(out, "note      no [tools] dashboard_url in %s; the dashboard agent uses %s.\n"+
			"          Add it to the profile so `pitf session` / `pitf model` find it too.\n", r.Path, opts.DashboardURL)
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
	fmt.Fprintf(out, "\nlogs: %s\nNext: `pitf up` starts agent-monitor; the page is http://127.0.0.1:8960/\n", m.Dirs.Logs)
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

// installedSpecs rebuilds the specs for status probes, taking the router's
// listen address from its installed plist so a custom --router-addr is
// probed where it actually listens.
func installedSpecs(m services.Manager) []services.Spec {
	opts := services.Options{RouterAddr: services.DefaultRouterAddr, DashboardAddr: services.DefaultDashboardAddr}
	if a, err := readPlist(m.Dirs.PlistPath("router")); err == nil {
		for i, arg := range a.Args {
			if arg == "-addr" && i+1 < len(a.Args) {
				opts.RouterAddr = a.Args[i+1]
			}
		}
	}
	return services.Plan(opts)
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

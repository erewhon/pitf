// Package cli builds the pitf root command and routes unknown subcommands to
// external pitf-<name> executables on PATH, the way git does.
package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/erewhon/pitf/internal/config"
)

// ExitError carries an exit status up to main. Msg, if set, is printed to
// stderr verbatim (no "pitf:" prefix) so a mounted tool's error reads the
// same as it does standalone; empty means the tool already reported.
type ExitError struct {
	Code int
	Msg  string
}

func (e *ExitError) Error() string {
	if e.Msg != "" {
		return e.Msg
	}
	return fmt.Sprintf("exit status %d", e.Code)
}

// Main is the whole program: it decides between a built-in command and an
// external one, then runs it. version is the build stamp shown by --version.
func Main(ctx context.Context, version string, args []string) error {
	gf := &globalFlags{}
	root := NewRoot(version, gf)

	// git-style dispatch: `pitf [global flags] foo …` with no built-in `foo`
	// runs `pitf-foo …`. Decided here, before cobra sees the args, so foo's
	// own flags are never parsed by the root command. Built-ins always win
	// over externals. Global flags before the name are honoured (they feed
	// the config the external inherits); anything after it is foo's.
	pre := pflag.NewFlagSet("pitf", pflag.ContinueOnError)
	pre.ParseErrorsAllowlist.UnknownFlags = true
	pre.SetInterspersed(false)
	pre.SetOutput(io.Discard)
	addGlobalFlags(pre, gf)
	_ = pre.Parse(args)
	if err := gf.loadEnvFiles(); err != nil {
		return err
	}
	if m, rest, ok := findMount(pre.Args()); ok {
		return runMount(ctx, m, version, gf, rest)
	}
	if name, rest, ok := externalCandidate(root, pre.Args()); ok {
		if path, found := lookupExternal(name, os.Getenv("PATH")); found {
			r, err := config.Resolve(gf.options())
			if err != nil {
				return err
			}
			if err := r.Apply(); err != nil {
				return err
			}
			return runExternal(ctx, path, rest)
		}
	}

	root.SetArgs(args)
	return root.ExecuteContext(ctx)
}

func addGlobalFlags(fs *pflag.FlagSet, gf *globalFlags) {
	fs.StringVar(&gf.profile, "profile", "", "config profile to use (overrides PITF_PROFILE and default_profile)")
	fs.StringVar(&gf.routerURL, "router-url", "", "router base URL (overrides PITF_ROUTER_URL and the profile)")
	fs.StringArrayVar(&gf.envFiles, "env-file", nil, "KEY=VALUE file loaded into the environment first (repeatable), e.g. upstream keys for `pitf router serve`")
}

// NewRoot builds the cobra root with every built-in subcommand attached.
func NewRoot(version string, gf *globalFlags) *cobra.Command {
	if gf == nil {
		gf = &globalFlags{}
	}
	root := &cobra.Command{
		Use:     "pitf",
		Short:   "One command over the smithy LLM tools",
		Long:    "pitf is one entry point over agent-monitor, tokenator, the llm-router\ncommands, and the Python ops tools. Anything it does not know is looked up\nas an executable named pitf-<name> on PATH and run with the remaining\narguments (git-style).",
		Version: version,
		// Root has no work of its own; an unknown first arg is an error that
		// also lists the externals we could see (see unknownCommand).
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return cmd.Help()
			}
			return unknownCommand(cmd, args[0])
		},
		// Root accepts arbitrary args so cobra defers unknown-command handling
		// to RunE instead of failing in its legacy validator.
		Args: cobra.ArbitraryArgs,
		PersistentPreRunE: func(*cobra.Command, []string) error {
			return gf.loadEnvFiles()
		},
	}
	root.SetVersionTemplate("pitf {{.Version}}\n")

	addGlobalFlags(root.PersistentFlags(), gf)
	root.AddCommand(newCompletionCmd(root), newConfigCmd(gf), newBenchCmd(gf), newSessionCmd(gf), newModelCmd(gf), newDashboardCmd(gf), newServicesCmd(gf), newUpCmd(version, gf))
	root.AddCommand(mountCommands()...)

	// Append discovered externals to `pitf help` / `pitf --help`.
	defaultHelp := root.HelpFunc()
	root.SetHelpFunc(func(cmd *cobra.Command, args []string) {
		defaultHelp(cmd, args)
		if cmd == root {
			writeExternalSection(cmd.OutOrStdout(), os.Getenv("PATH"))
		}
	})
	return root
}

// externalCandidate reports whether args name something that is not a
// built-in command or flag, i.e. a candidate for external dispatch.
func externalCandidate(root *cobra.Command, args []string) (name string, rest []string, ok bool) {
	if len(args) == 0 {
		return "", nil, false
	}
	name = args[0]
	if name == "" || strings.HasPrefix(name, "-") {
		return "", nil, false
	}
	if name == "help" {
		return "", nil, false
	}
	for _, c := range root.Commands() {
		if c.Name() == name || c.HasAlias(name) {
			return "", nil, false
		}
	}
	return name, args[1:], true
}

func unknownCommand(root *cobra.Command, name string) error {
	var b strings.Builder
	fmt.Fprintf(&b, "unknown command %q\n\nBuilt-in commands:\n", name)
	for _, c := range root.Commands() {
		if c.IsAvailableCommand() {
			fmt.Fprintf(&b, "  %s\n", c.Name())
		}
	}
	names := listExternals(os.Getenv("PATH"))
	if len(names) > 0 {
		fmt.Fprintf(&b, "\nExternal commands (pitf-<name> on PATH):\n")
		for _, n := range names {
			fmt.Fprintf(&b, "  %s\n", n)
		}
	} else {
		fmt.Fprintf(&b, "\nNo pitf-* executables found on PATH.")
	}
	return fmt.Errorf("%s", strings.TrimRight(b.String(), "\n"))
}

func writeExternalSection(w io.Writer, path string) {
	names := listExternals(path)
	if len(names) == 0 {
		return
	}
	fmt.Fprintf(w, "\nExternal Commands (pitf-<name> on PATH):\n")
	for _, n := range names {
		fmt.Fprintf(w, "  %s\n", n)
	}
}

func newCompletionCmd(root *cobra.Command) *cobra.Command {
	cmd := &cobra.Command{
		Use:       "completion [bash|zsh|fish]",
		Short:     "Print a shell completion script",
		Args:      cobra.MatchAll(cobra.ExactArgs(1), cobra.OnlyValidArgs),
		ValidArgs: []string{"bash", "zsh", "fish"},
		RunE: func(cmd *cobra.Command, args []string) error {
			w := cmd.OutOrStdout()
			switch args[0] {
			case "bash":
				return root.GenBashCompletionV2(w, true)
			case "zsh":
				return root.GenZshCompletion(w)
			default:
				return root.GenFishCompletion(w, true)
			}
		},
	}
	return cmd
}

// sortedKeys is a small helper shared by the external listing.
func sortedKeys(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortStrings(s []string) { sort.Strings(s) }

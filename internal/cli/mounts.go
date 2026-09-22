package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/erewhon/pitf/internal/config"
)

// A mount is a Go tool compiled into pitf and invoked like an external:
// `pitf [global flags] <name> <tool args…>`. Everything after the name is
// the tool's, parsed by the tool's own FlagSet. Mounts are resolved in Main
// before cobra, exactly like externals, so no tool ever sees pitf's flags
// and pitf never sees the tool's.
type mount struct {
	name  string
	short string
	// run is the tool's entrypoint. It must treat args as its own argv[1:].
	run func(ctx context.Context, args []string) error
	// signals: install a SIGINT/SIGTERM-cancelled context before run, as the
	// tool's own standalone wrapper does. Tools whose bodies install their
	// own handlers (or that never had one) leave this false so Ctrl-C keeps
	// its standalone meaning.
	signals bool
	// exit maps the tool's error to what the process should do. nil means
	// the default mapping (flag.ErrHelp → 0; anything else → 1 with the
	// message printed).
	exit func(err error) error
	// subs, when set, makes this a command group: args[0] must name one of
	// them and that sub's run/exit/signals are used. Unknown or missing sub
	// falls through to cobra for the group's help.
	subs []*mount
}

// mountRegistry is populated by mount_*.go init functions so each tool's
// import lives in its own file and can be dropped without touching this one.
var mountRegistry []*mount

func registerMount(m *mount) { mountRegistry = append(mountRegistry, m) }

func mountNames() []string {
	names := make([]string, 0, len(mountRegistry))
	for _, m := range mountRegistry {
		names = append(names, m.name)
	}
	sort.Strings(names)
	return names
}

// findMount resolves args (already stripped of global flags) to a runnable
// mount and the args it should receive. ok=false means "not a mount" or
// "a group with no valid sub", in which case cobra handles it.
func findMount(args []string) (m *mount, rest []string, ok bool) {
	if len(args) == 0 {
		return nil, nil, false
	}
	for _, cand := range mountRegistry {
		if cand.name != args[0] {
			continue
		}
		if cand.subs == nil {
			return cand, args[1:], true
		}
		if len(args) < 2 {
			return nil, nil, false
		}
		for _, sub := range cand.subs {
			if sub.name == args[1] {
				return sub, args[2:], true
			}
		}
		return nil, nil, false
	}
	return nil, nil, false
}

// runMount applies the resolved config to the process environment (the
// tools read env vars, not pitf's config), then runs the tool under its
// signal policy and maps its result to an exit.
func runMount(ctx context.Context, m *mount, version string, gf *globalFlags, args []string) error {
	r, err := config.Resolve(gf.options())
	if err != nil {
		return err
	}
	if err := r.Apply(); err != nil {
		return err
	}
	setMountVersions(version)
	if m.signals {
		var stop context.CancelFunc
		ctx, stop = signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
		defer stop()
	}
	err = m.run(ctx, args)
	if m.exit != nil {
		return m.exit(err)
	}
	return defaultExit(err)
}

// defaultExit: help is success; any other error is exit 1 with its text.
func defaultExit(err error) error {
	switch {
	case err == nil, errors.Is(err, flag.ErrHelp):
		return nil
	default:
		return &ExitError{Code: 1, Msg: err.Error()}
	}
}

// versionSetters are registered by mount_*.go so one build stamp reaches
// every tool's Version variable.
var versionSetters []func(string)

func setMountVersions(v string) {
	for _, set := range versionSetters {
		set(v)
	}
}

// mountCommands builds cobra stubs so `pitf help` lists the mounts and
// `pitf help monitor` says how to get the tool's own help. Invocation never
// goes through these (Main intercepts first), except a group name with no
// sub, which lands on the group's help.
func mountCommands() []*cobra.Command {
	var out []*cobra.Command
	for _, m := range mountRegistry {
		out = append(out, mountCommand(m, "pitf "+m.name))
	}
	return out
}

func mountCommand(m *mount, path string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   m.name,
		Short: m.short,
		Long: fmt.Sprintf("%s\n\nEverything after `%s` belongs to the tool: `%s --help` prints its own usage.\n"+
			"pitf's global flags (--profile, --router-url) go before the name.", m.short, path, path),
		DisableFlagParsing: true,
		SilenceUsage:       true,
	}
	if m.subs == nil {
		cmd.RunE = func(cmd *cobra.Command, args []string) error {
			// Only reachable if Main's interception was bypassed; keep it honest.
			return &ExitError{Code: 2, Msg: fmt.Sprintf("%s: internal dispatch miss", path)}
		}
		return cmd
	}
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
			names := make([]string, 0, len(m.subs))
			for _, s := range m.subs {
				names = append(names, s.name)
			}
			return &ExitError{Code: 2, Msg: fmt.Sprintf("%s: unknown subcommand %q (have: %s)", path, args[0], strings.Join(names, ", "))}
		}
		return cmd.Help()
	}
	for _, s := range m.subs {
		cmd.AddCommand(mountCommand(s, path+" "+s.name))
	}
	return cmd
}

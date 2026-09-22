package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/erewhon/pitf/internal/config"
)

// globalFlags are the root persistent flags that feed config resolution.
// They are parsed twice: once by hand before external dispatch (so
// `pitf --profile work bench …` works), once by cobra for built-ins.
type globalFlags struct {
	profile   string
	routerURL string
}

func (g *globalFlags) options() config.Options {
	return config.Options{Profile: g.profile, RouterURL: g.routerURL}
}

func newConfigCmd(gf *globalFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Show, locate, or export the resolved pitf config",
	}
	cmd.AddCommand(
		&cobra.Command{
			Use:   "path",
			Short: "Print the config file path",
			Args:  cobra.NoArgs,
			RunE: func(cmd *cobra.Command, _ []string) error {
				fmt.Fprintln(cmd.OutOrStdout(), config.DefaultPath(nil))
				return nil
			},
		},
		&cobra.Command{
			Use:   "show",
			Short: "Print the resolved config with secrets redacted",
			Args:  cobra.NoArgs,
			RunE: func(cmd *cobra.Command, _ []string) error {
				r, err := config.Resolve(gf.options())
				if err != nil {
					return err
				}
				return writeShow(cmd, r)
			},
		},
		&cobra.Command{
			Use:   "env",
			Short: "Print export lines for the resolved config (secrets in the clear)",
			Long:  "Prints `export K=V` lines, resolving api_key_cmd if needed, for `eval \"$(pitf config env)\"`.",
			Args:  cobra.NoArgs,
			RunE: func(cmd *cobra.Command, _ []string) error {
				r, err := config.Resolve(gf.options())
				if err != nil {
					return err
				}
				env, err := r.Environment()
				if err != nil {
					return err
				}
				for _, kv := range env {
					k, v, _ := strings.Cut(kv, "=")
					fmt.Fprintf(cmd.OutOrStdout(), "export %s=%s\n", k, shellQuote(v))
				}
				return nil
			},
		},
		&cobra.Command{
			Use:   "init",
			Short: "Write a starter config file (refuses to overwrite)",
			Args:  cobra.NoArgs,
			RunE: func(cmd *cobra.Command, _ []string) error {
				p := config.DefaultPath(nil)
				if _, err := os.Stat(p); err == nil {
					return fmt.Errorf("%s already exists", p)
				} else if !errors.Is(err, os.ErrNotExist) {
					return err
				}
				if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
					return err
				}
				if err := os.WriteFile(p, []byte(config.Example), 0o600); err != nil {
					return err
				}
				fmt.Fprintln(cmd.OutOrStdout(), "wrote", p)
				return nil
			},
		},
	)
	return cmd
}

func writeShow(cmd *cobra.Command, r *config.Resolved) error {
	w := cmd.OutOrStdout()
	state := "found"
	if !r.FileFound {
		state = "missing; `pitf config init` writes a starter"
	}
	fmt.Fprintf(w, "config:      %s (%s)\n", r.Path, state)
	if r.Profile != "" {
		fmt.Fprintf(w, "profile:     %s (from %s)\n", r.Profile, r.ProfileSource)
	} else {
		fmt.Fprintf(w, "profile:     (none)\n")
	}
	if len(r.Profiles) > 0 {
		fmt.Fprintf(w, "profiles:    %s\n", strings.Join(r.Profiles, ", "))
	}
	if r.RouterURL != "" {
		fmt.Fprintf(w, "router.url:  %s (from %s)\n", r.RouterURL, r.RouterURLSource)
	} else {
		fmt.Fprintf(w, "router.url:  (unset)\n")
	}
	switch {
	case r.KeyCmd() != "":
		fmt.Fprintf(w, "router.key:  via command %q (from %s; not run)\n", r.KeyCmd(), r.KeySource)
	case r.HasKey():
		fmt.Fprintf(w, "router.key:  set (from %s)\n", r.KeySource)
	default:
		fmt.Fprintf(w, "router.key:  (unset)\n")
	}
	fmt.Fprintf(w, "tools:       monitor %s · tokens %s · dashboard %s\n", orUnset(r.Tools.MonitorURL), orUnset(r.Tools.TokensURL), orUnset(r.Tools.DashboardURL))
	if len(r.Env) > 0 {
		keys := make([]string, 0, len(r.Env))
		for k := range r.Env {
			keys = append(keys, k)
		}
		sortStrings(keys)
		fmt.Fprintf(w, "env:\n")
		for _, k := range keys {
			fmt.Fprintf(w, "  %s=%s\n", k, r.Env[k])
		}
	}
	return nil
}

func orUnset(s string) string {
	if s == "" {
		return "(unset)"
	}
	return s
}

func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	if !strings.ContainsAny(s, " \t\n'\"\\$`!*?[]{}()<>|&;#~") {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

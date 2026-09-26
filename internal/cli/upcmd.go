package cli

import (
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/erewhon/pitf/internal/config"
)

func newUpCmd(version string, gf *globalFlags) *cobra.Command {
	var noMonitor, noOpen bool
	cmd := &cobra.Command{
		Use:   "up [-- agent-monitor flags…]",
		Short: "Make sure the launchd agents are running, open the router dashboard, then start agent-monitor",
		Long: "The one command for a working session on a laptop set up with\n" +
			"`pitf services install`: loads any agent that is stopped, waits for the\n" +
			"router to answer, prints the status, opens the router dashboard (the one\n" +
			"page: it proxies tokenator and agent-monitor), then runs `pitf monitor`\n" +
			"in this terminal. Anything after -- goes to agent-monitor.",
		RunE: func(cmd *cobra.Command, args []string) error {
			m, err := hostManager(cmd.OutOrStdout())
			if err != nil {
				return err
			}
			if err := m.Up(); err != nil {
				return err
			}
			specs := installedSpecs(m)
			if err := m.WaitFor(cmd.Context(), specs[0].URL, 10*time.Second); err != nil {
				cmd.PrintErrf("router not answering on %s yet: %v (see `pitf services status` and its log)\n", specs[0].URL, err)
			}
			if err := m.Status(cmd.Context(), specs); err != nil {
				return err
			}
			r, err := config.Resolve(gf.options())
			if err != nil {
				return err
			}
			home := dashboardHome(r, installedOptions(m).DashboardAddr)
			fmt.Fprintf(cmd.OutOrStdout(), "dashboard %s\n", home)
			if !noOpen {
				if err := openInBrowser(home); err != nil {
					cmd.PrintErrf("open %s: %v\n", home, err)
				}
			}
			if noMonitor {
				return nil
			}
			mon, rest, ok := findMount(append([]string{"monitor"}, args...))
			if !ok {
				return errNoMonitor
			}
			return runMount(cmd.Context(), mon, version, gf, rest)
		},
	}
	cmd.Flags().BoolVar(&noMonitor, "no-monitor", false, "only make sure the agents are running")
	cmd.Flags().BoolVar(&noOpen, "no-open", false, "do not open the router dashboard in the browser")
	return cmd
}

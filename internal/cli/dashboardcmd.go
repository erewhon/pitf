package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/erewhon/pitf/internal/config"
	"github.com/erewhon/pitf/internal/services"
)

// `pitf dashboard` was a loopback tab shell that framed agent-monitor,
// tokenator and the router dashboard. Retired 2026-09-26: the router
// dashboard proxies the other two itself (/monitor/, /tokens/) and is the
// one page, at home and on a laptop. This stub stays for one release so an
// old habit or script gets the address instead of "unknown command".
func newDashboardCmd(gf *globalFlags) *cobra.Command {
	var open bool
	cmd := &cobra.Command{
		Use:    "dashboard",
		Short:  "Retired: the router dashboard is the one page (prints its address)",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			r, err := config.Resolve(gf.options())
			if err != nil {
				return err
			}
			u := dashboardHome(r, services.DefaultDashboardAddr)
			fmt.Fprintf(cmd.OutOrStdout(), "pitf dashboard is retired: the router dashboard proxies tokenator (/tokens/) and agent-monitor (/monitor/) itself.\n%s\n", u)
			if open {
				return openInBrowser(u)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&open, "open", false, "open the router dashboard in the browser")
	return cmd
}

// dashboardHome is where the router dashboard lives for this config: the
// profile's [tools].dashboard_url, else the laptop router's own listener.
func dashboardHome(r *config.Resolved, dashboardAddr string) string {
	if r.Tools.DashboardURL != "" {
		return r.Tools.DashboardURL
	}
	return "http://" + dashboardAddr + "/"
}

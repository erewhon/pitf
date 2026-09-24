package cli

import (
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/spf13/cobra"

	"github.com/erewhon/pitf/internal/config"
	"github.com/erewhon/pitf/internal/dashboard"
)

func newDashboardCmd(gf *globalFlags) *cobra.Command {
	var listen string
	var open bool
	cmd := &cobra.Command{
		Use:   "dashboard",
		Short: "Serve one local page over agent-monitor, tokenator and the router dashboard",
		Long: "Serves a page on loopback with a tab per tool UI (agent-monitor,\n" +
			"tokenator, the router dashboard), framed from [tools] in the config.\n" +
			"A session id or model alias typed in the header points the tokens and\n" +
			"router tabs at that session or model. The page has no auth, so it only\n" +
			"listens on loopback. A router dashboard on loopback is framed too; a\n" +
			"remote one (behind SSO, which refuses framing) is a tab of links that\n" +
			"open in a new browser tab.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := dashboard.CheckLoopback(listen); err != nil {
				return err
			}
			r, err := config.Resolve(gf.options())
			if err != nil {
				return err
			}
			srv := &dashboard.Server{Links: linksFor(r), Probe: dashboard.HTTPProbe}
			ln, err := net.Listen("tcp", listen)
			if err != nil {
				return err
			}
			url := "http://" + ln.Addr().String() + "/"
			fmt.Fprintf(cmd.OutOrStdout(), "pitf dashboard on %s\n", url)
			if open {
				if err := openInBrowser(url); err != nil {
					fmt.Fprintf(cmd.ErrOrStderr(), "open: %v\n", err)
				}
			}

			// Ctrl-C ends the process (pitf sets no signal context); there is
			// no state to flush.
			hs := &http.Server{Handler: srv.Handler(), ReadHeaderTimeout: 10 * time.Second}
			return hs.Serve(ln)
		},
	}
	cmd.Flags().StringVar(&listen, "listen", dashboard.DefaultListen, "loopback address to listen on")
	cmd.Flags().BoolVar(&open, "open", false, "open the page in the browser")
	return cmd
}

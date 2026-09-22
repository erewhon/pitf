package cli

import (
	"errors"
	"flag"
	"os"

	amcli "github.com/erewhon/agent-monitor/cli"
)

func init() {
	versionSetters = append(versionSetters, func(v string) {
		amcli.Version = v
		// agent-monitor re-invokes itself inside its outer tmux (the TUI
		// pane, the placeholder pane, the status-bar stats); under pitf that
		// must come back through this mount, not the pitf root.
		if exe, err := os.Executable(); err == nil {
			amcli.SelfCommand = []string{exe, "monitor"}
		}
	})
	registerMount(&mount{
		name:    "monitor",
		short:   "agent-monitor: watch and drive tmux coding agents",
		run:     amcli.Run,
		signals: true, // agent-monitor's own wrapper installs one; the TUI and --web-only honour ctx
		exit: func(err error) error {
			var usage *amcli.UsageError
			switch {
			case err == nil, errors.Is(err, flag.ErrHelp):
				return nil
			case errors.As(err, &usage):
				return &ExitError{Code: 2} // usage already printed by the tool
			default:
				return &ExitError{Code: 1, Msg: "Error: " + err.Error()} // same prefix as standalone
			}
		},
	})
}

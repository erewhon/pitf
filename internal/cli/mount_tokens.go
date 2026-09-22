package cli

import (
	"errors"
	"flag"

	tcli "github.com/erewhon/tokenator/cli"
)

func init() {
	versionSetters = append(versionSetters, func(v string) { tcli.Version = v })
	registerMount(&mount{
		name:  "tokens",
		short: "tokenator: token profiler for coding-agent sessions",
		run:   tcli.Run,
		// No pitf-level signal context: `serve` does not watch ctx, and the
		// ctx-aware subcommands (otel, reqlog, bench run) install their own.
		// A pitf handler would swallow the first Ctrl-C on serve.
		signals: false,
		exit: func(err error) error {
			var usage *tcli.UsageError
			switch {
			case err == nil, errors.Is(err, flag.ErrHelp):
				return nil
			case errors.As(err, &usage):
				return &ExitError{Code: 2} // already reported by the tool
			default:
				return &ExitError{Code: 1, Msg: err.Error()} // tokenator prints err verbatim
			}
		},
	})
}

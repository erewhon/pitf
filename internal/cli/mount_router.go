package cli

import (
	"context"
	"errors"
	"flag"

	rcli "github.com/erewhon/llm-router-go/cli"
)

func init() {
	versionSetters = append(versionSetters, func(v string) { rcli.Version = v })
	// The standalone binaries exit 2 after --help (their long-standing
	// convention). Under pitf, help is success: the usage was printed and
	// the operator asked for it. Every other status passes through.
	exit := func(err error) error {
		switch {
		case err == nil, errors.Is(err, flag.ErrHelp):
			return nil
		}
		var ee *rcli.ExitError
		if errors.As(err, &ee) {
			return &ExitError{Code: ee.Code} // tool already reported
		}
		return &ExitError{Code: rcli.ExitCode(err), Msg: err.Error()}
	}
	sub := func(name, short string, run func(context.Context, []string) error) *mount {
		// Each body installs its own SIGINT/SIGTERM handling exactly where the
		// standalone binary does; gpu-exporter never had any. So no pitf-level
		// signal context here either.
		return &mount{name: name, short: short, run: run, exit: exit}
	}
	registerMount(&mount{
		name:  "router",
		short: "llm-router-go: the router and its fleet daemons",
		subs: []*mount{
			sub("serve", "the LLM router front door", rcli.Router),
			sub("node-agent", "per-host node agent (model servers, health, GPU)", rcli.NodeAgent),
			sub("gpu-exporter", "Prometheus GPU exporter", rcli.GPUExporter),
			sub("tool-proxy", "tool-call proxy", rcli.ToolProxy),
			sub("say", "orpheus-say: speak text through the router's TTS", rcli.Say),
		},
	})
}

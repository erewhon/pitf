package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/erewhon/pitf/internal/bench"
	"github.com/erewhon/pitf/internal/config"
)

func newBenchCmd(gf *globalFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "bench",
		Short: "pp/tg throughput sweeps against the router (streaming probe)",
		Long: "Measures prompt-processing and token-generation speed of models behind the\n" +
			"configured router using the streaming chat API. Numbers are a streaming probe\n" +
			"(pp = prompt_tokens/TTFT, tg = completion_tokens over the generation window),\n" +
			"NOT llama-bench; rows say so in Flags. The legacy multi-target comparison tool\n" +
			"(llm-router-bench) is still reachable as `pitf bench-py`.",
	}
	cmd.AddCommand(newBenchSweepCmd(gf), newBenchShowCmd(), newBenchImportCmd(gf))
	return cmd
}

func newBenchSweepCmd(gf *globalFlags) *cobra.Command {
	var (
		models   []string
		all      bool
		match    string
		pp       []int
		tg       int
		runs     int
		warmup   int
		host     string
		notes    string
		jsonOut  string
		registry string
		dryRun   bool
		timeout  time.Duration
		quiet    bool
	)
	cmd := &cobra.Command{
		Use:   "sweep",
		Short: "Run pp512/pp2048/tg128 legs for one or more model aliases",
		Example: "  pitf bench sweep --model glm-fast --model qwen38\n" +
			"  pitf --profile work bench sweep --all --match 'qwen*' --json rows.jsonl\n" +
			"  pitf bench sweep --model ling3 --registry ~/code/smithy/llm-router/models.yaml --dry-run",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			r, err := config.Resolve(gf.options())
			if err != nil {
				return err
			}
			if r.RouterURL == "" {
				return fmt.Errorf("no router URL: set [router].url in %s, PITF_ROUTER_URL, or --router-url", r.Path)
			}
			key, err := r.APIKey()
			if err != nil {
				return err
			}
			c := &bench.Client{BaseURL: r.RouterURL, APIKey: key}
			ctx := cmd.Context()
			if ctx == nil {
				ctx = context.Background()
			}
			if timeout > 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, timeout)
				defer cancel()
			}

			if all {
				list, err := c.ListModels(ctx)
				if err != nil {
					return err
				}
				for _, m := range list {
					if m.Role || m.Discovered || (m.APIClass != "" && m.APIClass != "chat") {
						continue
					}
					if match != "" {
						if ok, _ := filepath.Match(match, m.ID); !ok {
							continue
						}
					}
					models = append(models, m.ID)
				}
			}
			if len(models) == 0 {
				return fmt.Errorf("nothing to bench: pass --model <alias> (repeatable) or --all [--match glob]")
			}

			opts := bench.Options{Models: models, PP: pp, TG: tg, Runs: runs, Warmup: warmup, Host: host, Notes: notes}
			if !quiet {
				opts.Progress = cmd.ErrOrStderr()
			}
			if registry != "" {
				reg, err := bench.LoadRegistry(registry)
				if err != nil {
					return fmt.Errorf("--registry: %w", err)
				}
				opts.Registry = reg
			}

			if dryRun {
				fmt.Fprintf(cmd.OutOrStdout(), "router:  %s (profile %q)\nmodels:  %s\nlegs:    ", r.RouterURL, r.Profile, strings.Join(models, ", "))
				for _, p := range pp {
					fmt.Fprintf(cmd.OutOrStdout(), "pp%d ", p)
				}
				fmt.Fprintf(cmd.OutOrStdout(), "tg%d\nruns:    %d (+%d warmup) per leg\noutput:  table on stdout", tg, runs, warmup)
				if jsonOut != "" {
					fmt.Fprintf(cmd.OutOrStdout(), " + JSONL to %s", jsonOut)
				}
				fmt.Fprintln(cmd.OutOrStdout())
				return nil
			}

			rows, err := bench.Sweep(ctx, c, opts)
			if len(rows) > 0 {
				bench.WriteTable(cmd.OutOrStdout(), rows)
				if jsonOut != "" {
					if werr := writeJSONL(jsonOut, rows); werr != nil {
						return werr
					}
					fmt.Fprintf(cmd.ErrOrStderr(), "wrote %d row(s) to %s\n", len(rows), jsonOut)
				}
			}
			if err != nil {
				return err
			}
			for _, row := range rows {
				if row.Measure.Failed {
					return &ExitError{Code: 1, Msg: "one or more models failed every leg; see Notes"}
				}
			}
			return nil
		},
	}
	f := cmd.Flags()
	f.StringArrayVarP(&models, "model", "m", nil, "model alias to bench (repeatable)")
	f.BoolVar(&all, "all", false, "bench every non-discovered chat model the router lists")
	f.StringVar(&match, "match", "", "with --all: only aliases matching this glob")
	f.IntSliceVar(&pp, "pp", []int{512, 2048}, "prompt sizes (tokens) for the prompt-processing legs")
	f.IntVar(&tg, "tg", 128, "generation length (tokens) for the token-generation leg")
	f.IntVarP(&runs, "runs", "n", 3, "measured runs per leg (median reported)")
	f.IntVarP(&warmup, "warmup", "w", 1, "discarded warmup runs per model")
	f.StringVar(&host, "host", "", "Host column (default: the router's hostname; --registry overrides with the model's node)")
	f.StringVar(&notes, "notes", "", "text for the Notes column")
	f.StringVar(&jsonOut, "json", "", "also write rows as JSON lines to this file (- for stdout instead of the table)")
	f.StringVar(&registry, "registry", "", "models.yaml to fill HF Repo / Quant / Host / Engine / Context from")
	f.BoolVar(&dryRun, "dry-run", false, "print the plan and run nothing")
	f.DurationVar(&timeout, "timeout", 0, "overall deadline for the sweep (0 = none)")
	f.BoolVarP(&quiet, "quiet", "q", false, "no per-run progress on stderr")
	return cmd
}

func writeJSONL(path string, rows []bench.Row) error {
	if path == "-" {
		return bench.WriteJSONL(os.Stdout, rows)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	return bench.WriteJSONL(f, rows)
}

func newBenchShowCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "show <rows.jsonl>",
		Short: "Print the summary table for rows saved with --json",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			f, err := os.Open(args[0])
			if err != nil {
				return err
			}
			defer f.Close()
			rows, err := bench.ReadJSONL(f)
			if err != nil {
				return err
			}
			bench.WriteTable(cmd.OutOrStdout(), rows)
			return nil
		},
	}
}

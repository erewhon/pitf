package cli

import (
	"context"
	"fmt"
	"io"
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
			"NOT llama-bench; rows say so in Flags. The legacy Python comparison tool\n" +
			"(llm-router-bench) is retired from pitf; run it from the llm-router checkout\n" +
			"with `uv run llm-router-bench` if you still need it.",
	}
	cmd.AddCommand(newBenchSweepCmd(gf), newBenchShowCmd(), newBenchImportCmd(gf))
	return cmd
}

// benchSweep is bench.Sweep, as a variable so the command's tests can stand
// in canned rows for a real sweep.
var benchSweep = bench.Sweep

func newBenchSweepCmd(gf *globalFlags) *cobra.Command {
	var (
		forge    bool
		target   forgeTarget
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
		Long: "Runs the legs for each model and prints one Model Performance Matrix row per\n" +
			"model. With --forge the rows are also appended to the Matrix (the same path as\n" +
			"`pitf bench import`): the destination is resolved BEFORE the sweep starts, so a\n" +
			"missing [nous] config or a wrong name fails in a second rather than after the\n" +
			"run, and the rows are posted once, AFTER the sweep completes. A sweep that ends\n" +
			"in an error posts nothing; a model that failed every leg is skipped and the\n" +
			"others are posted. If the post itself fails the rows are kept in a JSONL file\n" +
			"for `pitf bench import`.",
		Example: "  pitf bench sweep --model glm-fast --model qwen38\n" +
			"  pitf bench sweep --model glm-fast --forge\n" +
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
				if forge {
					return printSweepForgePlan(cmd.OutOrStdout(), r, target, models, opts, c)
				}
				return nil
			}

			// The destination first: two GETs now beat a finished sweep
			// with nowhere to go.
			var sink *forgeSink
			if forge {
				if sink, err = openForgeSink(ctx, r, target); err != nil {
					return fmt.Errorf("--forge: %w (nothing was run)", err)
				}
			}

			rows, err := benchSweep(ctx, c, opts)
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
				if forge {
					fmt.Fprintln(cmd.ErrOrStderr(), "forge: the sweep did not complete; nothing posted")
				}
				return err
			}
			if forge {
				if err := postSweep(ctx, cmd.ErrOrStderr(), sink, rows, jsonOut); err != nil {
					return err
				}
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
	f.BoolVar(&dryRun, "dry-run", false, "print the plan and run nothing (with --forge: also what would be posted, and send nothing)")
	f.BoolVar(&forge, "forge", false, "append the finished rows to the Forge Model Performance Matrix, once, after the sweep completes")
	f.StringVar(&target.Notebook, "notebook", defaultForgeNotebook, "with --forge: Nous notebook name")
	f.StringVar(&target.Database, "database", defaultForgeDatabase, "with --forge: database title in that notebook")
	f.DurationVar(&timeout, "timeout", 0, "overall deadline for the sweep (0 = none)")
	f.BoolVarP(&quiet, "quiet", "q", false, "no per-run progress on stderr")
	return cmd
}

// printSweepForgePlan is --dry-run --forge: where the rows would go and what
// is already known of each. Nothing is sent, the daemon is not contacted.
func printSweepForgePlan(w io.Writer, r *config.Resolved, t forgeTarget, models []string, opts bench.Options, c *bench.Client) error {
	via := r.NousURL
	if !r.HasNous() {
		via = "NOT CONFIGURED — set [nous].url in " + r.Path + " or NOUS_DAEMON_URL; a real run would stop before the sweep"
	}
	fmt.Fprintf(w, "forge:   %s\n", via)
	rows := make([]bench.Row, 0, len(models))
	for _, m := range models {
		rows = append(rows, bench.PlanRow(m, opts, c))
	}
	_, payloads, _, err := forgePayloads(rows, true)
	if err != nil {
		return err
	}
	if err := printForgePlan(w, payloads, t); err != nil {
		return err
	}
	fmt.Fprintln(w, "the sweep adds the measured cells (pp/tg, Flags) to each row; a model that fails every leg is skipped")
	return nil
}

// postSweep sends a completed sweep's rows through the sink. When the post
// fails the measurements are not lost: they are in the --json file if one
// was given, else in a file written here, ready for `pitf bench import`.
func postSweep(ctx context.Context, w io.Writer, sink *forgeSink, rows []bench.Row, jsonOut string) error {
	keep, payloads, skipped, err := forgePayloads(rows, false)
	if err != nil {
		return err
	}
	for _, s := range skipped {
		fmt.Fprintf(w, "forge: skip %s\n", s)
	}
	if len(payloads) == 0 {
		fmt.Fprintln(w, "forge: no row to post")
		return nil
	}
	err = sink.post(ctx, w, keep, payloads)
	if err == nil {
		return nil
	}
	saved := jsonOut
	if saved == "" || saved == "-" {
		saved = "pitf-bench-" + time.Now().Format("20060102-150405") + ".jsonl"
		if werr := writeJSONL(saved, rows); werr != nil {
			return fmt.Errorf("forge: post failed: %w; saving the rows failed too: %v", err, werr)
		}
	}
	return fmt.Errorf("forge: post failed: %w; the rows are in %s — retry with `pitf bench import %s`", err, saved, saved)
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

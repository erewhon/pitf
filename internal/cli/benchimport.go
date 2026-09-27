package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/erewhon/pitf/internal/bench"
	"github.com/erewhon/pitf/internal/config"
	"github.com/erewhon/pitf/internal/nous"
)

// Where bench rows go unless told otherwise.
const (
	defaultForgeNotebook = "Forge"
	defaultForgeDatabase = "Model Performance Matrix"
)

// forgeTarget names the destination: a notebook and a database in it.
type forgeTarget struct{ Notebook, Database string }

// rowPayload turns a saved bench row into the property map Nous expects:
// matrix columns only (no "measure"), nil cells dropped, the method line
// folded into Flags when Flags is empty. Numbers stay numbers, dates stay
// YYYY-MM-DD strings, selects are option labels.
func rowPayload(r bench.Row) (map[string]any, error) {
	b, err := json.Marshal(r)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	delete(m, "measure")
	if s, _ := m["Flags"].(string); s == "" && r.Measure.Method != "" {
		m["Flags"] = r.Measure.Method
	}
	out := make(map[string]any, len(m))
	for k, v := range m {
		if v == nil {
			continue
		}
		if s, ok := v.(string); ok && s == "" {
			continue
		}
		out[k] = v
	}
	return out, nil
}

// selectRows applies the import filters and reports what was skipped.
func selectRows(rows []bench.Row, includeFailed bool) (keep []bench.Row, skipped []string) {
	for _, r := range rows {
		if r.Measure.Failed && !includeFailed {
			skipped = append(skipped, r.Model+" (failed every leg)")
			continue
		}
		keep = append(keep, r)
	}
	return keep, skipped
}

// forgePayloads is the one path from bench rows to what gets posted, shared
// by `bench import` and `bench sweep --forge`: filter, then shape.
func forgePayloads(rows []bench.Row, includeFailed bool) (keep []bench.Row, payloads []map[string]any, skipped []string, err error) {
	keep, skipped = selectRows(rows, includeFailed)
	payloads = make([]map[string]any, 0, len(keep))
	for _, r := range keep {
		p, err := rowPayload(r)
		if err != nil {
			return nil, nil, skipped, err
		}
		payloads = append(payloads, p)
	}
	return keep, payloads, skipped, nil
}

// printForgePlan is the --dry-run output: the rows exactly as they would be
// posted, and where.
func printForgePlan(w io.Writer, payloads []map[string]any, t forgeTarget) error {
	fmt.Fprintf(w, "would post %d row(s) to %q / %q:\n", len(payloads), t.Notebook, t.Database)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	for _, p := range payloads {
		if err := enc.Encode(p); err != nil {
			return err
		}
	}
	return nil
}

// forgeSink is a resolved destination. Opening one costs two GETs and
// proves the config, the daemon, the key and both names, so a caller with
// something slow to do first (a sweep) can find out before doing it.
type forgeSink struct {
	c   *nous.Client
	url string
	nb  nous.Notebook
	db  nous.Database
}

func openForgeSink(ctx context.Context, r *config.Resolved, t forgeTarget) (*forgeSink, error) {
	if !r.HasNous() {
		return nil, fmt.Errorf("no Nous daemon configured: set [nous].url in %s (or NOUS_DAEMON_URL); it is normally only reachable from home", r.Path)
	}
	key, err := r.NousAPIKey()
	if err != nil {
		return nil, err
	}
	s := &forgeSink{c: &nous.Client{BaseURL: r.NousURL, APIKey: key}, url: r.NousURL}
	if s.nb, err = s.c.ResolveNotebook(ctx, t.Notebook); err != nil {
		return nil, fmt.Errorf("%s: %w", r.NousURL, err)
	}
	if s.db, err = s.c.ResolveDatabase(ctx, s.nb.ID, t.Database); err != nil {
		return nil, err
	}
	return s, nil
}

// post appends the payloads and reports what landed on w. keep is the rows
// the payloads came from, for the model names in the report.
func (s *forgeSink) post(ctx context.Context, w io.Writer, keep []bench.Row, payloads []map[string]any) error {
	res, err := s.c.AddRows(ctx, s.nb.ID, s.db.ID, payloads)
	if err != nil {
		return err
	}
	models := make([]string, len(keep))
	for i, k := range keep {
		models[i] = k.Model
	}
	fmt.Fprintf(w, "added %d row(s) to %s / %s (now %d rows): %s\n", res.RowsAdded, s.nb.Name, s.db.Title, res.TotalRows, strings.Join(models, ", "))
	return nil
}

func newBenchImportCmd(gf *globalFlags) *cobra.Command {
	var (
		dryRun        bool
		includeFailed bool
		target        forgeTarget
	)
	cmd := &cobra.Command{
		Use:   "import <rows.jsonl>",
		Short: "Append rows saved with --json to the Forge Model Performance Matrix",
		Long: "Posts each row to the Nous database named by --database in the notebook named\n" +
			"by --notebook, through the daemon in [nous] (url + api_key/api_key_cmd), or\n" +
			"NOUS_DAEMON_URL / NOUS_API_KEY. Rows that failed every leg are skipped unless\n" +
			"--include-failed. Meant to be run from home on a file produced at work; where\n" +
			"the sweep itself runs at home, `pitf bench sweep --forge` does both in one step.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			f, err := os.Open(args[0])
			if err != nil {
				return err
			}
			defer f.Close()
			rows, err := bench.ReadJSONL(f)
			if err != nil {
				return fmt.Errorf("%s: %w", args[0], err)
			}
			keep, payloads, skipped, err := forgePayloads(rows, includeFailed)
			if err != nil {
				return err
			}
			w := cmd.OutOrStdout()
			for _, s := range skipped {
				fmt.Fprintf(cmd.ErrOrStderr(), "skip: %s\n", s)
			}
			if len(keep) == 0 {
				return fmt.Errorf("nothing to import from %s", args[0])
			}
			if dryRun {
				return printForgePlan(w, payloads, target)
			}

			r, err := config.Resolve(gf.options())
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			if ctx == nil {
				ctx = context.Background()
			}
			sink, err := openForgeSink(ctx, r, target)
			if err != nil {
				return err
			}
			return sink.post(ctx, w, keep, payloads)
		},
	}
	f := cmd.Flags()
	f.BoolVar(&dryRun, "dry-run", false, "print the rows as they would be posted and stop")
	f.BoolVar(&includeFailed, "include-failed", false, "also post rows whose every leg failed")
	f.StringVar(&target.Notebook, "notebook", defaultForgeNotebook, "Nous notebook name")
	f.StringVar(&target.Database, "database", defaultForgeDatabase, "database title in that notebook")
	return cmd
}

package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/erewhon/pitf/internal/bench"
	"github.com/erewhon/pitf/internal/config"
	"github.com/erewhon/pitf/internal/nous"
)

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

func newBenchImportCmd(gf *globalFlags) *cobra.Command {
	var (
		dryRun        bool
		includeFailed bool
		notebook      string
		database      string
	)
	cmd := &cobra.Command{
		Use:   "import <rows.jsonl>",
		Short: "Append rows saved with --json to the Forge Model Performance Matrix",
		Long: "Posts each row to the Nous database named by --database in the notebook named\n" +
			"by --notebook, through the daemon in [nous] (url + api_key/api_key_cmd), or\n" +
			"NOUS_DAEMON_URL / NOUS_API_KEY. Rows that failed every leg are skipped unless\n" +
			"--include-failed. Meant to be run from home on a file produced at work.",
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
			keep, skipped := selectRows(rows, includeFailed)
			w := cmd.OutOrStdout()
			for _, s := range skipped {
				fmt.Fprintf(cmd.ErrOrStderr(), "skip: %s\n", s)
			}
			if len(keep) == 0 {
				return fmt.Errorf("nothing to import from %s", args[0])
			}
			payloads := make([]map[string]any, 0, len(keep))
			for _, r := range keep {
				p, err := rowPayload(r)
				if err != nil {
					return err
				}
				payloads = append(payloads, p)
			}
			if dryRun {
				fmt.Fprintf(w, "would post %d row(s) to %q / %q:\n", len(payloads), notebook, database)
				enc := json.NewEncoder(w)
				enc.SetIndent("", "  ")
				for _, p := range payloads {
					if err := enc.Encode(p); err != nil {
						return err
					}
				}
				return nil
			}

			r, err := config.Resolve(gf.options())
			if err != nil {
				return err
			}
			if !r.HasNous() {
				return fmt.Errorf("no Nous daemon configured: set [nous].url in %s (or NOUS_DAEMON_URL); it is normally only reachable from home", r.Path)
			}
			key, err := r.NousAPIKey()
			if err != nil {
				return err
			}
			c := &nous.Client{BaseURL: r.NousURL, APIKey: key}
			ctx := cmd.Context()
			if ctx == nil {
				ctx = context.Background()
			}
			nb, err := c.ResolveNotebook(ctx, notebook)
			if err != nil {
				return fmt.Errorf("%s: %w", r.NousURL, err)
			}
			db, err := c.ResolveDatabase(ctx, nb.ID, database)
			if err != nil {
				return err
			}
			res, err := c.AddRows(ctx, nb.ID, db.ID, payloads)
			if err != nil {
				return err
			}
			models := make([]string, len(keep))
			for i, k := range keep {
				models[i] = k.Model
			}
			fmt.Fprintf(w, "added %d row(s) to %s / %s (now %d rows): %s\n", res.RowsAdded, nb.Name, db.Title, res.TotalRows, strings.Join(models, ", "))
			return nil
		},
	}
	f := cmd.Flags()
	f.BoolVar(&dryRun, "dry-run", false, "print the rows as they would be posted and stop")
	f.BoolVar(&includeFailed, "include-failed", false, "also post rows whose every leg failed")
	f.StringVar(&notebook, "notebook", "Forge", "Nous notebook name")
	f.StringVar(&database, "database", "Model Performance Matrix", "database title in that notebook")
	return cmd
}

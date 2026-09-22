package bench

import (
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"strings"
	"text/tabwriter"
	"time"
)

// Row is one Model Performance Matrix row. JSON keys are the matrix's
// property names verbatim so a saved file replays into Forge without a
// mapping step; "measure" carries the detail the matrix has no column for.
type Row struct {
	Model         string   `json:"Model"`
	HFRepo        string   `json:"HF Repo"`
	Quant         string   `json:"Quant"`
	Arch          string   `json:"Arch"`
	Params        string   `json:"Params"`
	FileSizeGB    *float64 `json:"File Size GB"`
	BPW           *float64 `json:"BPW"`
	Host          string   `json:"Host"`
	Accelerator   string   `json:"Accelerator"`
	Engine        string   `json:"Engine"`
	EngineVersion string   `json:"Engine Version"`
	Context       *int     `json:"Context"`
	Flags         string   `json:"Flags"`
	Speculative   string   `json:"Speculative"`
	PP512         *float64 `json:"pp512"`
	PP2048        *float64 `json:"pp2048"`
	TG128         *float64 `json:"tg128"`
	KVGiB         *float64 `json:"KV GiB"`
	TestDate      string   `json:"Test Date"`
	Verdict       string   `json:"Verdict"`
	Notes         string   `json:"Notes"`

	Measure Measure `json:"measure"`
}

// Measure is the non-matrix detail: how the numbers were taken.
type Measure struct {
	Method string `json:"method"`
	Router string `json:"router"`
	Alias  string `json:"alias"`
	Runs   int    `json:"runs"`
	Warmup int    `json:"warmup"`
	Legs   []Leg  `json:"legs"`
	Seed   int64  `json:"seed"`
	Failed bool   `json:"failed"`
}

const method = "streaming probe via OpenAI chat API; llama-server seats: server timings (prompt_n/prompt_ms, predicted_n/predicted_ms); others: prompt_tokens/TTFT and completion_tokens/(end-first token) at the client; NOT llama-bench"

// MatrixColumns is the matrix's property list, in order, for validation.
var MatrixColumns = []string{"Model", "HF Repo", "Quant", "Arch", "Params", "File Size GB", "BPW", "Host", "Accelerator", "Engine", "Engine Version", "Context", "Flags", "Speculative", "pp512", "pp2048", "tg128", "KV GiB", "Test Date", "Verdict", "Notes"}

func f64(v float64) *float64 {
	if v == 0 {
		return nil
	}
	x := v
	return &x
}

// NewRow shapes legs into a matrix row and applies registry enrichment.
func NewRow(alias string, legs []Leg, opts Options, c *Client) Row {
	host := opts.Host
	if host == "" {
		if u, err := url.Parse(c.BaseURL); err == nil {
			host = u.Hostname()
		}
	}
	r := Row{
		Model:    alias,
		Host:     host,
		TestDate: opts.Date.Format("2006-01-02"),
		Verdict:  "informational",
		Notes:    opts.Notes,
		Measure: Measure{
			Method: method, Router: c.BaseURL, Alias: alias,
			Runs: opts.Runs, Warmup: opts.Warmup, Legs: legs, Seed: opts.Seed,
		},
	}
	var flags []string
	flags = append(flags, fmt.Sprintf("pitf bench sweep: %d runs/leg, %d warmup, temp 0, max_tokens 16 (pp) / %d (tg); streaming probe, NOT llama-bench", opts.Runs, opts.Warmup, opts.TG))
	failed := 0
	for _, l := range legs {
		if l.Median == 0 {
			failed++
			continue
		}
		switch l.Name {
		case "pp512":
			r.PP512 = f64(round1(l.Median))
		case "pp2048":
			r.PP2048 = f64(round1(l.Median))
		case fmt.Sprintf("tg%d", opts.TG):
			r.TG128 = f64(round1(l.Median))
		}
		flags = append(flags, fmt.Sprintf("%s: %d tok, ttft %.0f ms, %.1f–%.1f t/s (%s)", l.Name, l.Tokens, l.TTFTms, l.Min, l.Max, l.Source))
	}
	if failed == len(legs) {
		r.Measure.Failed = true
		r.Verdict = "rejected"
		var errs []string
		for _, l := range legs {
			errs = append(errs, l.Errors...)
		}
		r.Notes = strings.TrimSpace(r.Notes + " FAILED: " + strings.Join(dedupe(errs), "; "))
	}
	r.Flags = strings.Join(flags, "; ")
	if opts.Registry != nil {
		opts.Registry.Enrich(&r)
	}
	return r
}

func round1(v float64) float64 { return float64(int(v*10+0.5)) / 10 }

func dedupe(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// WriteJSONL writes one JSON object per row.
func WriteJSONL(w io.Writer, rows []Row) error {
	enc := json.NewEncoder(w)
	for _, r := range rows {
		if err := enc.Encode(r); err != nil {
			return err
		}
	}
	return nil
}

// WriteTable prints the operator-facing summary.
func WriteTable(w io.Writer, rows []Row) {
	tw := tabwriter.NewWriter(w, 2, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "MODEL\tHOST\tpp512\tpp2048\ttg128\tTTFT(tg) ms\tQUANT\tNOTE")
	for _, r := range rows {
		ttft := ""
		for _, l := range r.Measure.Legs {
			if strings.HasPrefix(l.Name, "tg") && l.Median > 0 {
				ttft = fmt.Sprintf("%.0f", l.TTFTms)
			}
		}
		note := ""
		if r.Measure.Failed {
			note = "FAILED"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", r.Model, r.Host, num(r.PP512), num(r.PP2048), num(r.TG128), ttft, r.Quant, note)
	}
	tw.Flush()
}

func num(p *float64) string {
	if p == nil {
		return "-"
	}
	return fmt.Sprintf("%.1f", *p)
}

// ReadJSONL parses rows written by WriteJSONL (for import/replay).
func ReadJSONL(r io.Reader) ([]Row, error) {
	dec := json.NewDecoder(r)
	var rows []Row
	for {
		var row Row
		if err := dec.Decode(&row); err == io.EOF {
			return rows, nil
		} else if err != nil {
			return rows, err
		}
		rows = append(rows, row)
	}
}

var _ = time.Now

package bench

import (
	"context"
	"fmt"
	"io"
	"math/rand"
	"sort"
	"time"
)

// Options drive one sweep.
type Options struct {
	Models   []string // aliases to bench
	PP       []int    // prompt sizes for the pp legs (default 512, 2048)
	TG       int      // generation length for the tg leg (default 128)
	Runs     int      // measured runs per leg (default 3)
	Warmup   int      // discarded runs per model (default 1)
	Host     string   // Host column (default: the router URL's host)
	Notes    string   // free text for the Notes column
	Date     time.Time
	Seed     int64
	Progress io.Writer // per-run progress lines; nil for none
	Registry *Registry // optional models.yaml enrichment
}

func (o *Options) defaults() {
	if len(o.PP) == 0 {
		o.PP = []int{512, 2048}
	}
	if o.TG == 0 {
		o.TG = 128
	}
	if o.Runs == 0 {
		o.Runs = 3
	}
	if o.Date.IsZero() {
		o.Date = time.Now()
	}
	if o.Seed == 0 {
		o.Seed = time.Now().UnixNano()
	}
	if o.Progress == nil {
		o.Progress = io.Discard
	}
}

// Leg is one measured (model, prompt size) or (model, tg) cell.
type Leg struct {
	Name    string    `json:"name"` // "pp512", "pp2048", "tg128"
	Runs    int       `json:"runs"`
	Median  float64   `json:"median_tps"`
	Min     float64   `json:"min_tps"`
	Max     float64   `json:"max_tps"`
	Tokens  int       `json:"tokens"`  // actual prompt_tokens (pp) or completion_tokens (tg), median run
	TTFTms  float64   `json:"ttft_ms"` // median
	Errors  []string  `json:"errors,omitempty"`
	Samples []float64 `json:"samples_tps"`
	Source  string    `json:"source"` // "server timings" (llama-server) or "client" (usage + wall clock)
}

// Sweep benchmarks every model in opts and returns one Row per model.
func Sweep(ctx context.Context, c *Client, opts Options) ([]Row, error) {
	opts.defaults()
	rng := rand.New(rand.NewSource(opts.Seed))
	var rows []Row
	for _, model := range opts.Models {
		if ctx.Err() != nil {
			return rows, ctx.Err()
		}
		fmt.Fprintf(opts.Progress, "%s\n", model)
		for i := 0; i < opts.Warmup; i++ {
			fmt.Fprintf(opts.Progress, "  warmup %d/%d…", i+1, opts.Warmup)
			s := c.Complete(ctx, model, BuildPrompt(rng, 64), 16)
			if s.Err != "" {
				fmt.Fprintf(opts.Progress, " error: %s\n", s.Err)
			} else {
				fmt.Fprintf(opts.Progress, " ok\n")
			}
		}
		var legs []Leg
		for _, pp := range opts.PP {
			legs = append(legs, runLeg(ctx, c, opts, rng, model, fmt.Sprintf("pp%d", pp), pp, 16, true))
		}
		legs = append(legs, runLeg(ctx, c, opts, rng, model, fmt.Sprintf("tg%d", opts.TG), 0, opts.TG, false))
		rows = append(rows, NewRow(model, legs, opts, c))
	}
	return rows, nil
}

func runLeg(ctx context.Context, c *Client, opts Options, rng *rand.Rand, model, name string, promptTokens, maxTokens int, prompt bool) Leg {
	leg := Leg{Name: name, Runs: opts.Runs}
	type rec struct {
		tps  float64
		tok  int
		ttft float64
	}
	var ok []rec
	for i := 0; i < opts.Runs; i++ {
		if ctx.Err() != nil {
			leg.Errors = append(leg.Errors, ctx.Err().Error())
			break
		}
		fmt.Fprintf(opts.Progress, "  %s run %d/%d…", name, i+1, opts.Runs)
		var text string
		if prompt {
			text = BuildPrompt(rng, promptTokens)
		} else {
			text = BuildGenPrompt(rng)
		}
		s := c.Complete(ctx, model, text, maxTokens)
		if s.Err == "" && !s.HasTimings && s.Buffered() {
			// Every token arrived in one burst at the end: a non-streaming
			// proxy (the router's tool proxy does this) sat in between.
			// Server timings survive that; client-side rates do not.
			s.Err = fmt.Sprintf("buffered stream (%d tokens arrived together after %.0f ms): a non-streaming proxy is in the path; bench a direct seat or one that reports server timings", s.CompletionTokens, float64(s.TTFT.Milliseconds()))
		}
		if s.Err == "" && !prompt && s.CompletionTokens*4 < maxTokens*3 {
			// The model stopped well before the cut-off; too few tokens for a
			// steady-state decode figure.
			s.Err = fmt.Sprintf("short generation: %d of %d tokens", s.CompletionTokens, maxTokens)
		}
		if s.Err != "" {
			leg.Errors = append(leg.Errors, s.Err)
			fmt.Fprintf(opts.Progress, " error: %s\n", s.Err)
			continue
		}
		var r rec
		if prompt {
			n := s.PromptTokens
			if s.HasTimings {
				n = s.ProcessedPrompt
			}
			r = rec{s.PromptTPS(), n, float64(s.TTFT.Microseconds()) / 1000}
		} else {
			r = rec{s.GenTPS(), s.CompletionTokens, float64(s.TTFT.Microseconds()) / 1000}
		}
		if r.tps == 0 {
			leg.Errors = append(leg.Errors, "unmeasurable run (no tokens or zero time)")
			fmt.Fprintf(opts.Progress, " unmeasurable\n")
			continue
		}
		ok = append(ok, r)
		leg.Samples = append(leg.Samples, r.tps)
		leg.Source = s.Source()
		fmt.Fprintf(opts.Progress, " %.1f t/s (%d tok, ttft %.0f ms)\n", r.tps, r.tok, r.ttft)
	}
	if len(ok) == 0 {
		return leg
	}
	sort.Slice(ok, func(i, j int) bool { return ok[i].tps < ok[j].tps })
	m := ok[(len(ok)-1)/2] // lower median on even counts: conservative
	leg.Median, leg.Min, leg.Max = m.tps, ok[0].tps, ok[len(ok)-1].tps
	leg.Tokens, leg.TTFTms = m.tok, m.ttft
	return leg
}

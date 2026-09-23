package cli

import (
	"strings"
	"testing"

	"github.com/erewhon/pitf/internal/bench"
)

func f64p(v float64) *float64 { return &v }

func TestRowPayloadShape(t *testing.T) {
	r := bench.Row{Model: "qwen38-27b", Host: "talos", PP512: f64p(234.3), TG128: f64p(20.2), TestDate: "2026-09-22", Verdict: "informational",
		Measure: bench.Measure{Method: "streaming probe", Legs: []bench.Leg{{Name: "tg128"}}}}
	p, err := rowPayload(r)
	if err != nil {
		t.Fatal(err)
	}
	if _, has := p["measure"]; has {
		t.Fatal("measure must not be posted")
	}
	for _, absent := range []string{"pp2048", "KV GiB", "HF Repo", "Notes"} {
		if _, has := p[absent]; has {
			t.Fatalf("empty cell %q must be dropped, got %v", absent, p[absent])
		}
	}
	if p["pp512"] != 234.3 || p["tg128"] != 20.2 || p["Model"] != "qwen38-27b" || p["Test Date"] != "2026-09-22" {
		t.Fatalf("payload: %v", p)
	}
	if p["Flags"] != "streaming probe" {
		t.Fatalf("empty Flags should carry the method line, got %v", p["Flags"])
	}
	r.Flags = "explicit"
	p, _ = rowPayload(r)
	if p["Flags"] != "explicit" {
		t.Fatal("non-empty Flags must be kept as is")
	}
}

func TestSelectRowsSkipsFailed(t *testing.T) {
	rows := []bench.Row{{Model: "ok"}, {Model: "bad", Measure: bench.Measure{Failed: true}}}
	keep, skipped := selectRows(rows, false)
	if len(keep) != 1 || keep[0].Model != "ok" || len(skipped) != 1 || !strings.Contains(skipped[0], "bad") {
		t.Fatalf("keep=%v skipped=%v", keep, skipped)
	}
	keep, skipped = selectRows(rows, true)
	if len(keep) != 2 || len(skipped) != 0 {
		t.Fatal("--include-failed should keep both")
	}
}

package bench

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakeServer streams a completion with a fixed TTFT and per-token delay,
// reporting prompt_tokens from the request's character count so the
// prompt-size legs see different token counts.
func fakeServer(t *testing.T, ttft, perTok time.Duration, reasoning bool) (*httptest.Server, *int32) {
	return fakeServerMode(t, ttft, perTok, reasoning, false)
}

// fakeServerMode with timings=true mimics llama-server: no usage chunk,
// but a "timings" object on the final chunk.
func fakeServerMode(t *testing.T, ttft, perTok time.Duration, reasoning, timings bool) (*httptest.Server, *int32) {
	t.Helper()
	var calls int32
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer k" {
			http.Error(w, "unauthorized", 401)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"data": []Model{
			{ID: "alpha", OwnedBy: "vllm"},
			{ID: "coder", OwnedBy: "vllm", Role: true},
			{ID: "cloud-x", OwnedBy: "openrouter", Discovered: true},
			{ID: "embed", OwnedBy: "llama", APIClass: "embeddings"},
			{ID: "qwen-a", OwnedBy: "vllm", APIClass: "chat"},
		}})
	})
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		var req struct {
			MaxTokens int `json:"max_tokens"`
			Messages  []struct {
				Content string `json:"content"`
			} `json:"messages"`
			StreamOptions map[string]bool `json:"stream_options"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		if !req.StreamOptions["include_usage"] {
			http.Error(w, "need include_usage", 400)
			return
		}
		promptTok := len(req.Messages[0].Content) / 4
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		time.Sleep(ttft)
		for i := 0; i < req.MaxTokens; i++ {
			delta := map[string]any{"content": "x"}
			if reasoning && i < req.MaxTokens/2 {
				delta = map[string]any{"content": nil, "reasoning_content": "think"}
			}
			fmt.Fprintf(w, "data: %s\n\n", must(json.Marshal(map[string]any{"choices": []any{map[string]any{"delta": delta}}})))
			fl.Flush()
			time.Sleep(perTok)
		}
		if timings {
			fmt.Fprintf(w, "data: %s\n\n", must(json.Marshal(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{}, "finish_reason": "length"}},
				"timings": map[string]any{"cache_n": 0, "prompt_n": promptTok, "prompt_ms": 100.0, "predicted_n": req.MaxTokens, "predicted_ms": float64(req.MaxTokens) * 10}})))
		} else {
			fmt.Fprintf(w, "data: %s\n\n", must(json.Marshal(map[string]any{"choices": []any{}, "usage": map[string]int{"prompt_tokens": promptTok, "completion_tokens": req.MaxTokens}})))
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
		fl.Flush()
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, &calls
}

func must(b []byte, err error) []byte {
	if err != nil {
		panic(err)
	}
	return b
}

func TestCompleteTimesReasoningDeltaAsFirstToken(t *testing.T) {
	srv, _ := fakeServer(t, 40*time.Millisecond, 2*time.Millisecond, true)
	c := &Client{BaseURL: srv.URL, APIKey: "k"}
	s := c.Complete(context.Background(), "alpha", strings.Repeat("word ", 100), 20)
	if s.Err != "" {
		t.Fatal(s.Err)
	}
	if s.TTFT < 40*time.Millisecond || s.TTFT > 80*time.Millisecond {
		t.Fatalf("TTFT %v should be the first reasoning delta (~40ms), not the first content delta", s.TTFT)
	}
	if s.CompletionTokens != 20 || s.PromptTokens == 0 || s.GenTPS() <= 0 || s.PromptTPS() <= 0 {
		t.Fatalf("sample = %+v", s)
	}
}

func TestBuildPromptIsUncachableAndSized(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	a, b := BuildPrompt(rng, 512), BuildPrompt(rng, 512)
	if a == b || a[:16] == b[:16] {
		t.Fatal("two prompts share a prefix; prompt caches would shortcut the prefill")
	}
	if n := len(a) / 4; n < 480 || n > 640 {
		t.Fatalf("512-token prompt is ~%d tokens by the 4-char rule", n)
	}
}

func TestSweepProducesMatrixRowsAndJSONL(t *testing.T) {
	srv, calls := fakeServer(t, 20*time.Millisecond, time.Millisecond, false)
	c := &Client{BaseURL: srv.URL, APIKey: "k"}
	var progress bytes.Buffer
	rows, err := Sweep(context.Background(), c, Options{
		Models: []string{"alpha"}, PP: []int{512, 2048}, TG: 32, Runs: 3, Warmup: 1,
		Date: time.Date(2026, 9, 22, 0, 0, 0, 0, time.UTC), Seed: 7, Progress: &progress, Notes: "unit",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt32(calls); got != 1+3*3 {
		t.Fatalf("expected 1 warmup + 3 legs x 3 runs = 10 requests, got %d", got)
	}
	r := rows[0]
	if r.PP512 == nil || r.PP2048 == nil || r.TG128 == nil || r.Measure.Failed {
		t.Fatalf("row incomplete: %+v", r)
	}
	if *r.PP2048 <= *r.PP512 {
		t.Fatalf("with fixed TTFT a bigger prompt must show higher pp t/s: pp512=%v pp2048=%v", *r.PP512, *r.PP2048)
	}
	if r.TestDate != "2026-09-22" || r.Host != "127.0.0.1" || !strings.Contains(r.Flags, "NOT llama-bench") {
		t.Fatalf("row meta: date=%q host=%q flags=%q", r.TestDate, r.Host, r.Flags)
	}
	if !strings.Contains(progress.String(), "pp2048 run 3/3") {
		t.Fatalf("progress missing: %s", progress.String())
	}

	// JSON keys must be exactly the matrix columns (plus "measure").
	var buf bytes.Buffer
	if err := WriteJSONL(&buf, rows); err != nil {
		t.Fatal(err)
	}
	var obj map[string]any
	if err := json.Unmarshal(buf.Bytes(), &obj); err != nil {
		t.Fatal(err)
	}
	for _, col := range MatrixColumns {
		if _, ok := obj[col]; !ok {
			t.Errorf("JSON row lacks matrix column %q", col)
		}
	}
	if len(obj) != len(MatrixColumns)+1 {
		t.Errorf("JSON row has %d keys, want %d matrix columns + measure", len(obj), len(MatrixColumns)+1)
	}
	back, err := ReadJSONL(&buf)
	if err != nil || len(back) != 1 || *back[0].TG128 != *r.TG128 {
		t.Fatalf("round trip: %v %+v", err, back)
	}
	var table bytes.Buffer
	WriteTable(&table, rows)
	if !strings.Contains(table.String(), "alpha") || !strings.Contains(table.String(), "pp2048") {
		t.Fatalf("table: %s", table.String())
	}
}

func TestSweepMarksFailedModel(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { http.Error(w, `{"error":"no such model"}`, 404) })
	bad := httptest.NewServer(mux)
	defer bad.Close()
	rows, err := Sweep(context.Background(), &Client{BaseURL: bad.URL}, Options{Models: []string{"nope"}, Runs: 1, Warmup: 0})
	if err != nil {
		t.Fatal(err)
	}
	if !rows[0].Measure.Failed || rows[0].Verdict != "rejected" || !strings.Contains(rows[0].Notes, "no such model") {
		t.Fatalf("failed row: %+v", rows[0])
	}
}

func TestListModelsAuthAndFields(t *testing.T) {
	srv, _ := fakeServer(t, 0, 0, false)
	if _, err := (&Client{BaseURL: srv.URL}).ListModels(context.Background()); err == nil {
		t.Fatal("expected 401 without key")
	}
	list, err := (&Client{BaseURL: srv.URL + "/v1", APIKey: "k"}).ListModels(context.Background())
	if err != nil || len(list) != 5 {
		t.Fatalf("list: %v %v", err, list)
	}
}

func TestRegistryEnrich(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "models.yaml")
	os.WriteFile(p, []byte(`
nodes:
  cottus: {host: 1.2.3.4, gpu: nvidia}
models:
  ling3:
    hf_repo: inclusionAI/Ling-3.0-flash
    gguf_file: /tank/models/Ling-3.0-flash-GGUF/Ling-3.0-flash-UD-Q4_K_XL.gguf
    node: cottus
    backend: vllm
    aliases: [ling-fast]
    context_length: 131072
  gpt: {hf_repo: openai/gpt-oss-120b, gguf_file: /x/gpt-oss-120b-MXFP4.gguf}
  glm-spark:
    hf_repo: RedHatAI/GLM-5.3-Flash-NVFP4
    backend: vllm
    aliases: [glm-fast]
    multi_node: {nodes: [archimedes, hypatia], tensor_parallel_size: 2, head_node: archimedes}
`), 0o644)
	reg, err := LoadRegistry(p)
	if err != nil {
		t.Fatal(err)
	}
	r := Row{Model: "ling-fast", Host: "llm.bcc.sh"}
	reg.Enrich(&r)
	if r.HFRepo != "inclusionAI/Ling-3.0-flash" || r.Quant != "UD-Q4_K_XL" || r.Host != "cottus" || r.Engine != "llama.cpp" || r.Context == nil || *r.Context != 131072 || !strings.Contains(r.Notes, "alias of ling3") {
		t.Fatalf("enriched: %+v", r)
	}
	g := Row{Model: "gpt"}
	reg.Enrich(&g)
	if g.Quant != "MXFP4" || g.Host != "" {
		t.Fatalf("gpt: %+v", g)
	}
	m := Row{Model: "glm-fast", Host: "llm.bcc.sh"}
	reg.Enrich(&m)
	if m.Host != "archimedes+hypatia" || m.Quant != "NVFP4" || m.Engine != "vllm" || !strings.Contains(m.Notes, "TP=2") {
		t.Fatalf("multi-node: %+v", m)
	}
	u := Row{Model: "unknown", Host: "h"}
	reg.Enrich(&u)
	if u.Host != "h" || u.HFRepo != "" {
		t.Fatalf("unknown alias must be untouched: %+v", u)
	}
}

func TestCompletePrefersServerTimings(t *testing.T) {
	srv, _ := fakeServerMode(t, 30*time.Millisecond, time.Millisecond, false, true)
	c := &Client{BaseURL: srv.URL, APIKey: "k"}
	s := c.Complete(context.Background(), "alpha", strings.Repeat("word ", 200), 20)
	if s.Err != "" || !s.HasTimings || s.Source() != "server timings" {
		t.Fatalf("sample = %+v", s)
	}
	// prompt_ms fixed at 100 ms → pp = prompt_n / 0.1 s; predicted 10 ms/token → exactly 100 t/s.
	if want := float64(s.ProcessedPrompt) / 0.1; s.PromptTPS() != want {
		t.Fatalf("PromptTPS = %v, want %v from server timings", s.PromptTPS(), want)
	}
	if s.GenTPS() != 100 {
		t.Fatalf("GenTPS = %v, want 100 from server timings", s.GenTPS())
	}
	rows, err := Sweep(context.Background(), c, Options{Models: []string{"alpha"}, Runs: 1, Warmup: 0, TG: 20})
	if err != nil || rows[0].Measure.Failed || rows[0].Measure.Legs[0].Source != "server timings" || !strings.Contains(rows[0].Flags, "(server timings)") {
		t.Fatalf("rows: %v %+v", err, rows[0])
	}
}

func TestPrefixCacheTolerance(t *testing.T) {
	mk := func(cacheN, promptN int) Sample {
		return sampleFromTimings(t, cacheN, promptN)
	}
	if s := mk(42, 500); s.Err != "" || s.ProcessedPrompt != 500 || s.PromptTokens != 542 {
		t.Fatalf("template-header cache hit must be tolerated: %+v", s)
	}
	s := mk(400, 100)
	if !s.LargeCacheHit() {
		t.Fatalf("400 of 500 cached must count as a large hit: %+v", s)
	}
	// With no warmup baseline a large hit voids the run, as before.
	if err := acceptCachedPrefix(s, 0); err == nil || !strings.Contains(err.Error(), "no warmup baseline") {
		t.Fatalf("large hit without baseline: %v", err)
	}
	// Matching the warmup's hit (within slack) is a constant injected prefix.
	if err := acceptCachedPrefix(s, 404); err != nil {
		t.Fatalf("constant prefix must be accepted: %v", err)
	}
	// A hit that changed size is the prompt itself being cached.
	if err := acceptCachedPrefix(s, 300); err == nil || !strings.Contains(err.Error(), "differs from the warmup") {
		t.Fatalf("changed prefix: %v", err)
	}
	// Same size, but almost nothing left to prefill: still a cached prompt.
	if err := acceptCachedPrefix(mk(723, 10), 723); err == nil || !strings.Contains(err.Error(), "uncached") {
		t.Fatalf("tiny remainder: %v", err)
	}
}

// fakePrefixServer mimics llama-server behind the tool proxy: every request
// reports the same cached prefix (cache_n) and prefills the rest.
func fakePrefixServer(t *testing.T, cacheN func(i int) int) *httptest.Server {
	t.Helper()
	var n int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
			MaxTokens int `json:"max_tokens"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		i := int(atomic.AddInt32(&n, 1)) - 1
		promptN := len(req.Messages[0].Content) / 4
		w.Header().Set("Content-Type", "text/event-stream")
		for k := 0; k < req.MaxTokens; k++ {
			fmt.Fprintf(w, "data: %s\n\n", must(json.Marshal(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"content": "x"}}}})))
		}
		fmt.Fprintf(w, "data: %s\n\n", must(json.Marshal(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{}}},
			"timings": map[string]any{"cache_n": cacheN(i), "prompt_n": promptN, "prompt_ms": 100.0, "predicted_n": req.MaxTokens, "predicted_ms": float64(req.MaxTokens) * 10}})))
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestSweepAcceptsConstantCachedPrefix(t *testing.T) {
	srv := fakePrefixServer(t, func(int) int { return 723 })
	c := &Client{BaseURL: srv.URL, APIKey: "k"}
	rows, err := Sweep(context.Background(), c, Options{Models: []string{"qwen3.8-27b"}, Runs: 2, Warmup: 1, PP: []int{512}, TG: 20})
	if err != nil || rows[0].Measure.Failed {
		t.Fatalf("constant 723-token prefix must be accepted: %v %+v", err, rows[0])
	}
	r := rows[0]
	if r.PP512 == nil || r.TG128 == nil {
		t.Fatalf("legs missing: %+v", r)
	}
	if !strings.Contains(r.Notes, "Cached prefix 723 tok") {
		t.Fatalf("notes should record the prefix: %q", r.Notes)
	}
	// pp counts only the uncached tokens: prompt_n over prompt_ms.
	if l := r.Measure.Legs[0]; l.CachedPrefix != 723 || l.Tokens >= 723 {
		t.Fatalf("pp leg = %+v, want cached_prefix 723 and uncached token count", l)
	}
}

func TestSweepRejectsGrowingCachedPrefix(t *testing.T) {
	// Warmup sees 723 cached; measured runs see much more — a cached prompt.
	srv := fakePrefixServer(t, func(i int) int { return 723 + i*400 })
	c := &Client{BaseURL: srv.URL, APIKey: "k"}
	rows, _ := Sweep(context.Background(), c, Options{Models: []string{"m"}, Runs: 1, Warmup: 1, PP: []int{512}, TG: 20})
	if !rows[0].Measure.Failed || !strings.Contains(rows[0].Notes, "differs from the warmup") {
		t.Fatalf("growing prefix must fail every leg: %+v", rows[0])
	}
}

func TestSweepRejectsLargeHitWithoutWarmup(t *testing.T) {
	srv := fakePrefixServer(t, func(int) int { return 723 })
	c := &Client{BaseURL: srv.URL, APIKey: "k"}
	rows, _ := Sweep(context.Background(), c, Options{Models: []string{"m"}, Runs: 1, Warmup: 0, PP: []int{512}, TG: 20})
	if !rows[0].Measure.Failed || !strings.Contains(rows[0].Notes, "no warmup baseline") {
		t.Fatalf("no baseline must keep the old refusal: %+v", rows[0])
	}
}

// sampleFromTimings runs Complete against a one-shot server that reports
// the given llama-server timings.
func sampleFromTimings(t *testing.T, cacheN, promptN int) Sample {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: %s\n\n", must(json.Marshal(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"content": "x"}}}})))
		fmt.Fprintf(w, "data: %s\n\n", must(json.Marshal(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{}}},
			"timings": map[string]any{"cache_n": cacheN, "prompt_n": promptN, "prompt_ms": 50.0, "predicted_n": 4, "predicted_ms": 40.0}})))
		fmt.Fprint(w, "data: [DONE]\n\n")
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	return (&Client{BaseURL: srv.URL}).Complete(context.Background(), "m", "p", 4)
}

func TestShortGenerationIsFlagged(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: %s\n\n", must(json.Marshal(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"content": "OK"}}}})))
		fmt.Fprintf(w, "data: %s\n\n", must(json.Marshal(map[string]any{"choices": []any{}, "usage": map[string]int{"prompt_tokens": 30, "completion_tokens": 2}})))
		fmt.Fprint(w, "data: [DONE]\n\n")
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	rows, _ := Sweep(context.Background(), &Client{BaseURL: srv.URL}, Options{Models: []string{"m"}, PP: []int{64}, TG: 128, Runs: 1, Warmup: 0})
	var tg Leg
	for _, l := range rows[0].Measure.Legs {
		if l.Name == "tg128" {
			tg = l
		}
	}
	if tg.Median != 0 || len(tg.Errors) != 1 || !strings.Contains(tg.Errors[0], "short generation: 2 of 128") {
		t.Fatalf("tg leg should be void on a 2-token answer: %+v", tg)
	}
	if rows[0].PP512 != nil && rows[0].Measure.Legs[0].Median == 0 {
		t.Fatalf("pp leg should still count: %+v", rows[0].Measure.Legs[0])
	}
}

func TestBufferedStreamVoidsClientLegs(t *testing.T) {
	// Proxy signature: long silence, then every token at once, usage-only.
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		time.Sleep(60 * time.Millisecond)
		for i := 0; i < 64; i++ {
			fmt.Fprintf(w, "data: %s\n\n", must(json.Marshal(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"content": "x"}}}})))
		}
		fmt.Fprintf(w, "data: %s\n\n", must(json.Marshal(map[string]any{"choices": []any{}, "usage": map[string]int{"prompt_tokens": 500, "completion_tokens": 64}})))
		fmt.Fprint(w, "data: [DONE]\n\n")
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	rows, _ := Sweep(context.Background(), &Client{BaseURL: srv.URL}, Options{Models: []string{"m"}, PP: []int{512}, TG: 64, Runs: 1, Warmup: 0})
	for _, l := range rows[0].Measure.Legs {
		if l.Median != 0 || len(l.Errors) == 0 || !strings.Contains(l.Errors[0], "buffered stream") {
			t.Fatalf("leg %s should be void behind a buffering proxy: %+v", l.Name, l)
		}
	}
}

func TestLowerMedianOnEvenRuns(t *testing.T) {
	// alternate fast/slow responses; with 2 runs the reported value must be the slower one.
	var n int32
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		k := atomic.AddInt32(&n, 1)
		pms := 100.0
		if k%2 == 0 {
			pms = 300.0
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: %s\n\n", must(json.Marshal(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"content": "x"}}}})))
		fmt.Fprintf(w, "data: %s\n\n", must(json.Marshal(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{}}},
			"timings": map[string]any{"cache_n": 0, "prompt_n": 300, "prompt_ms": pms, "predicted_n": 16, "predicted_ms": 160.0}})))
		fmt.Fprint(w, "data: [DONE]\n\n")
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	rows, _ := Sweep(context.Background(), &Client{BaseURL: srv.URL}, Options{Models: []string{"m"}, PP: []int{300}, TG: 16, Runs: 2, Warmup: 0})
	if got := rows[0].Measure.Legs[0].Median; got != 1000 {
		t.Fatalf("pp median over {3000, 1000} with 2 runs should be the lower 1000, got %v", got)
	}
}

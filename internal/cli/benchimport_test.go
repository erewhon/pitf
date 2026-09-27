package cli

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
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

// ---------------------------------------------------------------------------
// bench sweep --forge
// ---------------------------------------------------------------------------

// fakeForge is a Nous daemon that records, in order, every request it gets
// and every row posted to the Matrix. failPost makes the POST answer 500.
type fakeForge struct {
	mu       sync.Mutex
	order    *[]string
	posted   []map[string]any
	posts    int
	failPost bool
}

func newFakeForge(t *testing.T, order *[]string) (*httptest.Server, *fakeForge) {
	t.Helper()
	ff := &fakeForge{order: order}
	note := func(s string) {
		ff.mu.Lock()
		*ff.order = append(*ff.order, s)
		ff.mu.Unlock()
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/notebooks", func(w http.ResponseWriter, r *http.Request) {
		note("nous: notebooks")
		if r.Header.Get("Authorization") != "Bearer nous-key" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"unauthorized"}`))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]string{{"id": "nb1", "name": "Forge"}, {"id": "nb2", "name": "Personal"}}})
	})
	mux.HandleFunc("GET /api/notebooks/nb1/databases", func(w http.ResponseWriter, r *http.Request) {
		note("nous: databases")
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{{"id": "db1", "title": "Model Performance Matrix", "rowCount": 88}, {"id": "db2", "title": "Project Tasks"}}})
	})
	mux.HandleFunc("POST /api/notebooks/nb1/databases/db1/rows", func(w http.ResponseWriter, r *http.Request) {
		note("nous: post rows")
		var body struct {
			Rows []map[string]any `json:"rows"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		ff.mu.Lock()
		ff.posts++
		fail := ff.failPost
		if !fail {
			ff.posted = append(ff.posted, body.Rows...)
		}
		ff.mu.Unlock()
		if fail {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":"database is locked"}`))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"databaseId": "db1", "rowsAdded": len(body.Rows), "totalRows": 88 + len(body.Rows)}})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, ff
}

// sweepEnv points pitf at a config with a router (never contacted: the sweep
// is stubbed) and, when nousURL is set, a Nous daemon; it isolates the run
// from the ambient environment and moves into an empty directory.
func sweepEnv(t *testing.T, nousURL string) string {
	t.Helper()
	dir := t.TempDir()
	cfg := "[router]\nurl = \"http://router.invalid:4010\"\napi_key = \"router-key\"\n"
	if nousURL != "" {
		cfg += "\n[nous]\nurl = \"" + nousURL + "\"\napi_key = \"nous-key\"\n"
	}
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PITF_CONFIG", path)
	t.Setenv("HOME", dir)
	for _, k := range []string{"PITF_PROFILE", "PITF_ROUTER_URL", "PITF_ROUTER_API_KEY", "PITF_NOUS_URL", "PITF_NOUS_API_KEY", "NOUS_DAEMON_URL", "NOUS_API_KEY",
		"LLM_ROUTER_URL", "LLM_ROUTER_API_KEY", "OPENAI_BASE_URL", "OPENAI_API_KEY"} {
		t.Setenv(k, "")
	}
	t.Chdir(dir)
	return dir
}

// stubSweep replaces the real sweep with canned rows (one per model asked
// for; a model named "bad…" failed every leg) and records that it ran.
func stubSweep(t *testing.T, order *[]string, sweepErr error) *int {
	t.Helper()
	calls := 0
	old := benchSweep
	benchSweep = func(_ context.Context, c *bench.Client, opts bench.Options) ([]bench.Row, error) {
		calls++
		*order = append(*order, "sweep")
		var rows []bench.Row
		for _, m := range opts.Models {
			r := bench.Row{Model: m, Host: "talos", TestDate: "2026-09-26", Verdict: "informational", Flags: "pitf bench sweep: 3 runs/leg",
				PP512: f64p(234.3), PP2048: f64p(470.1), TG128: f64p(20.2),
				Measure: bench.Measure{Method: "streaming probe", Router: c.BaseURL, Alias: m, Runs: 3}}
			if strings.HasPrefix(m, "bad") {
				r.PP512, r.PP2048, r.TG128 = nil, nil, nil
				r.Verdict, r.Notes, r.Measure.Failed = "rejected", "FAILED: 502", true
			}
			rows = append(rows, r)
		}
		return rows, sweepErr
	}
	t.Cleanup(func() { benchSweep = old })
	return &calls
}

func TestSweepForgePostsMatrixRowsAfterTheSweep(t *testing.T) {
	var order []string
	srv, ff := newFakeForge(t, &order)
	sweepEnv(t, srv.URL)
	calls := stubSweep(t, &order, nil)

	out, err := runPitf(t, "bench", "sweep", "--model", "qwen38-27b", "--model", "bad-seat", "--forge", "-q")
	// The failed model still fails the command, after the good row is in.
	var ee *ExitError
	if !errors.As(err, &ee) || ee.Code != 1 {
		t.Fatalf("err = %v, want the failed-model exit; output:\n%s", err, out)
	}
	if *calls != 1 || ff.posts != 1 {
		t.Fatalf("sweeps = %d, posts = %d, want 1 and 1", *calls, ff.posts)
	}
	// Destination resolved before the sweep; rows posted once, after it.
	if got := strings.Join(order, " | "); got != "nous: notebooks | nous: databases | sweep | nous: post rows" {
		t.Errorf("order = %s", got)
	}
	if len(ff.posted) != 1 {
		t.Fatalf("posted %d rows, want 1 (the failed model is skipped): %v", len(ff.posted), ff.posted)
	}
	row := ff.posted[0]
	columns := map[string]bool{}
	for _, c := range bench.MatrixColumns {
		columns[c] = true
	}
	for k := range row {
		if !columns[k] {
			t.Errorf("posted key %q is not a Matrix column", k)
		}
	}
	if row["Model"] != "qwen38-27b" || row["Host"] != "talos" || row["pp512"] != 234.3 || row["pp2048"] != 470.1 || row["tg128"] != 20.2 ||
		row["Test Date"] != "2026-09-26" || row["Verdict"] != "informational" {
		t.Errorf("posted row = %v", row)
	}
	for _, want := range []string{"forge: skip bad-seat (failed every leg)", "added 1 row(s) to Forge / Model Performance Matrix (now 89 rows): qwen38-27b"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
}

func TestSweepForgeDryRunSendsNothing(t *testing.T) {
	var order []string
	srv, ff := newFakeForge(t, &order)
	sweepEnv(t, srv.URL)
	calls := stubSweep(t, &order, nil)

	out, err := runPitf(t, "bench", "sweep", "--model", "qwen38-27b", "--model", "glm-fast", "--forge", "--dry-run", "--notes", "after the b11010 bump")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if len(order) != 0 || *calls != 0 || ff.posts != 0 {
		t.Fatalf("dry run touched something: order=%v sweeps=%d posts=%d", order, *calls, ff.posts)
	}
	for _, want := range []string{"forge:   " + srv.URL, `would post 2 row(s) to "Forge" / "Model Performance Matrix"`, `"Model": "qwen38-27b"`, `"Model": "glm-fast"`,
		`"Host": "router.invalid"`, `"Notes": "after the b11010 bump"`, "the sweep adds the measured cells"} {
		if !strings.Contains(out, want) {
			t.Errorf("dry-run output lacks %q:\n%s", want, out)
		}
	}
	for _, absent := range []string{"pp512", "FAILED", "rejected", "measure"} {
		if strings.Contains(out, `"`+absent) {
			t.Errorf("dry-run rows must not invent %q:\n%s", absent, out)
		}
	}
}

func TestSweepForgeDryRunSaysWhenNousIsMissing(t *testing.T) {
	var order []string
	sweepEnv(t, "")
	stubSweep(t, &order, nil)
	out, err := runPitf(t, "bench", "sweep", "--model", "m", "--forge", "--dry-run")
	if err != nil || !strings.Contains(out, "forge:   NOT CONFIGURED") {
		t.Fatalf("err=%v out:\n%s", err, out)
	}
}

func TestSweepForgeStopsBeforeTheSweepWithoutADestination(t *testing.T) {
	t.Run("no nous configured", func(t *testing.T) {
		var order []string
		sweepEnv(t, "")
		calls := stubSweep(t, &order, nil)
		_, err := runPitf(t, "bench", "sweep", "--model", "m", "--forge", "-q")
		if err == nil || !strings.Contains(err.Error(), "no Nous daemon configured") || !strings.Contains(err.Error(), "nothing was run") || *calls != 0 {
			t.Fatalf("err = %v, sweeps = %d", err, *calls)
		}
	})
	t.Run("unknown database", func(t *testing.T) {
		var order []string
		srv, ff := newFakeForge(t, &order)
		sweepEnv(t, srv.URL)
		calls := stubSweep(t, &order, nil)
		_, err := runPitf(t, "bench", "sweep", "--model", "m", "--forge", "--database", "No Such Matrix", "-q")
		if err == nil || !strings.Contains(err.Error(), `database "No Such Matrix"`) || *calls != 0 || ff.posts != 0 {
			t.Fatalf("err = %v, sweeps = %d, posts = %d", err, *calls, ff.posts)
		}
	})
}

func TestSweepForgeFailedSweepPostsNothing(t *testing.T) {
	var order []string
	srv, ff := newFakeForge(t, &order)
	sweepEnv(t, srv.URL)
	stubSweep(t, &order, context.DeadlineExceeded) // rows for what finished, and an error

	out, err := runPitf(t, "bench", "sweep", "--model", "qwen38-27b", "--forge", "-q")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v", err)
	}
	if ff.posts != 0 {
		t.Fatalf("a failed sweep posted %d time(s)", ff.posts)
	}
	if !strings.Contains(out, "the sweep did not complete; nothing posted") || !strings.Contains(out, "qwen38-27b") {
		t.Errorf("output should say so and still show the partial table:\n%s", out)
	}
}

func TestSweepForgeKeepsTheRowsWhenThePostFails(t *testing.T) {
	var order []string
	srv, ff := newFakeForge(t, &order)
	ff.failPost = true
	dir := sweepEnv(t, srv.URL)
	stubSweep(t, &order, nil)

	_, err := runPitf(t, "bench", "sweep", "--model", "qwen38-27b", "--forge", "-q")
	if err == nil || !strings.Contains(err.Error(), "database is locked") || !strings.Contains(err.Error(), "pitf bench import pitf-bench-") {
		t.Fatalf("err = %v", err)
	}
	saved, _ := filepath.Glob(filepath.Join(dir, "pitf-bench-*.jsonl"))
	if len(saved) != 1 {
		t.Fatalf("fallback files = %v", saved)
	}
	f, err := os.Open(saved[0])
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	rows, err := bench.ReadJSONL(f)
	if err != nil || len(rows) != 1 || rows[0].Model != "qwen38-27b" || rows[0].TG128 == nil {
		t.Fatalf("saved rows = %+v, err = %v", rows, err)
	}

	// With --json the rows are already on disk: no second file, and the
	// error points at the one the user asked for.
	_, err = runPitf(t, "bench", "sweep", "--model", "qwen38-27b", "--forge", "--json", "mine.jsonl", "-q")
	if err == nil || !strings.Contains(err.Error(), "pitf bench import mine.jsonl") {
		t.Fatalf("err = %v", err)
	}
	if again, _ := filepath.Glob(filepath.Join(dir, "pitf-bench-*.jsonl")); len(again) != 1 {
		t.Errorf("a fallback file was written although --json was given: %v", again)
	}
}

func TestSweepWithoutForgeNeverTouchesNous(t *testing.T) {
	var order []string
	srv, ff := newFakeForge(t, &order)
	sweepEnv(t, srv.URL)
	stubSweep(t, &order, nil)
	if out, err := runPitf(t, "bench", "sweep", "--model", "qwen38-27b", "-q"); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if strings.Join(order, ",") != "sweep" || ff.posts != 0 {
		t.Fatalf("order = %v, posts = %d", order, ff.posts)
	}
}

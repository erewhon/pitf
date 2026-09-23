package nous

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func fake(t *testing.T) (*httptest.Server, *[]map[string]any) {
	t.Helper()
	var posted []map[string]any
	mux := http.NewServeMux()
	auth := func(w http.ResponseWriter, r *http.Request) bool {
		if r.Header.Get("Authorization") != "Bearer k" {
			w.WriteHeader(401)
			w.Write([]byte(`{"error":"unauthorized"}`))
			return false
		}
		return true
	}
	mux.HandleFunc("GET /api/notebooks", func(w http.ResponseWriter, r *http.Request) {
		if !auth(w, r) {
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"data": []Notebook{{ID: "nb1", Name: "Forge"}, {ID: "nb2", Name: "Forge Archive"}, {ID: "nb3", Name: "Personal"}}})
	})
	mux.HandleFunc("GET /api/notebooks/nb1/databases", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"data": []Database{{ID: "db1", Title: "Model Performance Matrix", RowCount: 88}, {ID: "db2", Title: "Project Tasks"}}})
	})
	mux.HandleFunc("POST /api/notebooks/nb1/databases/db1/rows", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Rows []map[string]any `json:"rows"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		posted = append(posted, body.Rows...)
		json.NewEncoder(w).Encode(map[string]any{"data": AddResult{DatabaseID: "db1", RowsAdded: len(body.Rows), TotalRows: 88 + len(body.Rows)}})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, &posted
}

func TestResolveAndAdd(t *testing.T) {
	srv, posted := fake(t)
	c := &Client{BaseURL: srv.URL + "/", APIKey: "k"}
	ctx := context.Background()
	nb, err := c.ResolveNotebook(ctx, "Forge")
	if err != nil || nb.ID != "nb1" {
		t.Fatalf("exact name: %v %v", nb, err)
	}
	if nb, err := c.ResolveNotebook(ctx, "pers"); err != nil || nb.ID != "nb3" {
		t.Fatalf("prefix: %v %v", nb, err)
	}
	if _, err := c.ResolveNotebook(ctx, "forge a"); err != nil {
		t.Fatalf("unique prefix should resolve: %v", err)
	}
	if _, err := c.ResolveNotebook(ctx, "nope"); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("missing: %v", err)
	}
	db, err := c.ResolveDatabase(ctx, "nb1", "model perf")
	if err != nil || db.ID != "db1" {
		t.Fatalf("db: %v %v", db, err)
	}
	res, err := c.AddRows(ctx, "nb1", "db1", []map[string]any{{"Model": "x", "tg128": 20.2}})
	if err != nil || res.RowsAdded != 1 || res.TotalRows != 89 || len(*posted) != 1 || (*posted)[0]["Model"] != "x" {
		t.Fatalf("add: %+v %v posted=%v", res, err, *posted)
	}
}

func TestErrorsCarryDaemonMessage(t *testing.T) {
	srv, _ := fake(t)
	_, err := (&Client{BaseURL: srv.URL}).Notebooks(context.Background())
	if err == nil || !strings.Contains(err.Error(), "401: unauthorized") {
		t.Fatalf("want daemon error text, got %v", err)
	}
}

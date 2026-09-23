// Package nous is the small slice of the Nous daemon HTTP API pitf needs:
// resolve a notebook and a database by name, and add rows. It mirrors
// nous-py's daemon_client: Bearer auth, a {"data": …} envelope, and
// {"error": …} bodies on failure.
package nous

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Client talks to one Nous daemon (local http://localhost:7667 or the hosted
// app). APIKey is a PAT or rw daemon key.
type Client struct {
	BaseURL string
	APIKey  string
	HTTP    *http.Client
}

// Notebook is a row of GET /api/notebooks.
type Notebook struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// Database is a row of GET /api/notebooks/{id}/databases.
type Database struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	RowCount int    `json:"rowCount"`
}

// AddResult is the envelope payload of POST …/rows.
type AddResult struct {
	DatabaseID string `json:"databaseId"`
	RowsAdded  int    `json:"rowsAdded"`
	TotalRows  int    `json:"totalRows"`
}

func (c *Client) do(ctx context.Context, method, path string, body any, out any) error {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(c.BaseURL, "/")+path, rdr)
	if err != nil {
		return err
	}
	req.Header.Set("X-Nous-Client", "pitf")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.APIKey)
	}
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode >= 400 {
		var e struct {
			Error string `json:"error"`
		}
		msg := strings.TrimSpace(string(raw))
		if json.Unmarshal(raw, &e) == nil && e.Error != "" {
			msg = e.Error
		}
		return fmt.Errorf("nous %s %s: %d: %s", method, path, resp.StatusCode, msg)
	}
	if out == nil {
		return nil
	}
	var env struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err == nil && len(env.Data) > 0 {
		return json.Unmarshal(env.Data, out)
	}
	return json.Unmarshal(raw, out)
}

// Notebooks lists the notebooks the key can see.
func (c *Client) Notebooks(ctx context.Context) ([]Notebook, error) {
	var out []Notebook
	return out, c.do(ctx, http.MethodGet, "/api/notebooks", nil, &out)
}

// Databases lists a notebook's databases.
func (c *Client) Databases(ctx context.Context, notebookID string) ([]Database, error) {
	var out []Database
	return out, c.do(ctx, http.MethodGet, "/api/notebooks/"+notebookID+"/databases", nil, &out)
}

// AddRows appends rows (property name → value) to a database.
func (c *Client) AddRows(ctx context.Context, notebookID, databaseID string, rows []map[string]any) (AddResult, error) {
	var out AddResult
	return out, c.do(ctx, http.MethodPost, "/api/notebooks/"+notebookID+"/databases/"+databaseID+"/rows",
		map[string]any{"rows": rows}, &out)
}

// ResolveNotebook finds a notebook by exact name, then case-insensitive
// prefix (unique), then id.
func (c *Client) ResolveNotebook(ctx context.Context, name string) (Notebook, error) {
	nbs, err := c.Notebooks(ctx)
	if err != nil {
		return Notebook{}, err
	}
	names := make([]string, len(nbs))
	for i, n := range nbs {
		names[i] = n.Name
	}
	i, err := match(name, names, func(j int) string { return nbs[j].ID })
	if err != nil {
		return Notebook{}, fmt.Errorf("notebook %q: %w", name, err)
	}
	return nbs[i], nil
}

// ResolveDatabase finds a database in a notebook the same way, by title.
func (c *Client) ResolveDatabase(ctx context.Context, notebookID, title string) (Database, error) {
	dbs, err := c.Databases(ctx, notebookID)
	if err != nil {
		return Database{}, err
	}
	titles := make([]string, len(dbs))
	for i, d := range dbs {
		titles[i] = d.Title
	}
	i, err := match(title, titles, func(j int) string { return dbs[j].ID })
	if err != nil {
		return Database{}, fmt.Errorf("database %q: %w", title, err)
	}
	return dbs[i], nil
}

// match is the resolution rule shared by notebooks and databases.
func match(want string, names []string, id func(int) string) (int, error) {
	for i, n := range names {
		if n == want || id(i) == want {
			return i, nil
		}
	}
	lw := strings.ToLower(want)
	var hits []int
	for i, n := range names {
		if strings.HasPrefix(strings.ToLower(n), lw) {
			hits = append(hits, i)
		}
	}
	switch len(hits) {
	case 1:
		return hits[0], nil
	case 0:
		return -1, fmt.Errorf("not found (have: %s)", strings.Join(names, ", "))
	default:
		amb := make([]string, len(hits))
		for k, h := range hits {
			amb[k] = names[h]
		}
		return -1, fmt.Errorf("ambiguous: %s", strings.Join(amb, ", "))
	}
}

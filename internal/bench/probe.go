// Package bench measures prompt-processing and token-generation throughput
// of models behind an OpenAI-compatible endpoint (the router), using the
// streaming chat API. It is a streaming probe, NOT llama-bench: prompt
// speed is prompt_tokens / time-to-first-token, generation speed is
// completion_tokens / time from first token to the end. Rows say so.
package bench

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"strings"
	"time"
)

// Client talks to one OpenAI-compatible base URL.
type Client struct {
	BaseURL string // e.g. https://llm.bcc.sh (with or without /v1)
	APIKey  string
	HTTP    *http.Client
}

func (c *Client) v1() string {
	u := strings.TrimRight(c.BaseURL, "/")
	if !strings.HasSuffix(u, "/v1") {
		u += "/v1"
	}
	return u
}

func (c *Client) http() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: 10 * time.Minute}
}

func (c *Client) do(ctx context.Context, method, path string, body any) (*http.Response, error) {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.v1()+path, rdr)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.APIKey)
	}
	return c.http().Do(req)
}

// Model is one entry of GET /v1/models, with the router's extra fields.
type Model struct {
	ID         string `json:"id"`
	OwnedBy    string `json:"owned_by"`
	APIClass   string `json:"api_class,omitempty"`
	Role       bool   `json:"role,omitempty"`
	Discovered bool   `json:"discovered,omitempty"`
}

// ListModels returns GET /v1/models.
func (c *Client) ListModels(ctx context.Context) ([]Model, error) {
	resp, err := c.do(ctx, http.MethodGet, "/models", nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("GET /v1/models: %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	var out struct {
		Data []Model `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return out.Data, nil
}

// Sample is one streamed completion's timing.
type Sample struct {
	PromptTokens     int
	CompletionTokens int
	TTFT             time.Duration // request start → first delta
	Total            time.Duration // request start → stream end
	Err              string

	// Server-side timings, present when the backend is llama-server (its
	// final chunk carries "timings"). These exclude the network and the
	// router hop, so they are preferred over the client-derived rates.
	HasTimings      bool
	PromptMs        float64
	PredMs          float64
	CacheN          int // prompt tokens served from the prefix cache (template header, normally)
	ProcessedPrompt int // prompt_n: tokens actually prefilled this request
}

// Buffered reports whether the whole completion landed in one burst: more
// than a handful of tokens, yet the first-token-to-end window is under 5%
// of the total. That is the signature of a proxy that streams only after
// it has the full answer.
func (s Sample) Buffered() bool {
	if s.CompletionTokens < 8 || s.Total <= 0 {
		return false
	}
	return (s.Total - s.TTFT) < s.Total/20
}

// Source names where the rates came from.
func (s Sample) Source() string {
	if s.HasTimings {
		return "server timings"
	}
	return "client"
}

// PromptTPS is the prefill rate: server prompt_n/prompt_ms when available,
// else prompt_tokens / TTFT. Zero when unmeasurable.
func (s Sample) PromptTPS() float64 {
	if s.HasTimings {
		if s.ProcessedPrompt == 0 || s.PromptMs <= 0 {
			return 0
		}
		return float64(s.ProcessedPrompt) / (s.PromptMs / 1000)
	}
	if s.PromptTokens == 0 || s.TTFT <= 0 {
		return 0
	}
	return float64(s.PromptTokens) / s.TTFT.Seconds()
}

// GenTPS is the decode rate: server predicted_n/predicted_ms when
// available, else completion_tokens / (Total - TTFT). Zero when unmeasurable.
func (s Sample) GenTPS() float64 {
	if s.HasTimings {
		if s.CompletionTokens == 0 || s.PredMs <= 0 {
			return 0
		}
		return float64(s.CompletionTokens) / (s.PredMs / 1000)
	}
	gen := s.Total - s.TTFT
	if s.CompletionTokens == 0 || gen <= 0 {
		return 0
	}
	return float64(s.CompletionTokens) / gen.Seconds()
}

// Complete streams one chat completion and times it. The first delta of
// any kind (content, reasoning, tool call) counts as the first token, so
// thinking models are timed from when they start emitting, not from when
// visible text appears.
func (c *Client) Complete(ctx context.Context, model, prompt string, maxTokens int) Sample {
	payload := map[string]any{
		"model":          model,
		"messages":       []map[string]string{{"role": "user", "content": prompt}},
		"max_tokens":     maxTokens,
		"temperature":    0,
		"stream":         true,
		"stream_options": map[string]bool{"include_usage": true},
	}
	start := time.Now()
	resp, err := c.do(ctx, http.MethodPost, "/chat/completions", payload)
	if err != nil {
		return Sample{Total: time.Since(start), Err: err.Error()}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return Sample{Total: time.Since(start), Err: fmt.Sprintf("%s: %s", resp.Status, strings.TrimSpace(string(b)))}
	}
	var s Sample
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			break
		}
		var chunk struct {
			Choices []struct {
				Delta map[string]json.RawMessage `json:"delta"`
			} `json:"choices"`
			Usage *struct {
				PromptTokens     int `json:"prompt_tokens"`
				CompletionTokens int `json:"completion_tokens"`
			} `json:"usage"`
			Timings *struct {
				PromptN  int     `json:"prompt_n"`
				PromptMs float64 `json:"prompt_ms"`
				PredN    int     `json:"predicted_n"`
				PredMs   float64 `json:"predicted_ms"`
				CacheN   int     `json:"cache_n"`
			} `json:"timings"`
		}
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			continue
		}
		if s.TTFT == 0 && len(chunk.Choices) > 0 && deltaHasTokens(chunk.Choices[0].Delta) {
			s.TTFT = time.Since(start)
		}
		if chunk.Usage != nil {
			s.PromptTokens = chunk.Usage.PromptTokens
			s.CompletionTokens = chunk.Usage.CompletionTokens
		}
		if t := chunk.Timings; t != nil && t.PromptMs > 0 {
			s.HasTimings = true
			s.PromptMs, s.PredMs, s.CacheN = t.PromptMs, t.PredMs, t.CacheN
			// llama-server sends timings instead of a usage chunk; the
			// counts are the same thing. prompt_n excludes cached tokens.
			if s.PromptTokens == 0 {
				s.PromptTokens = t.PromptN + t.CacheN
			}
			s.ProcessedPrompt = t.PromptN
			if s.CompletionTokens == 0 {
				s.CompletionTokens = t.PredN
			}
		}
	}
	s.Total = time.Since(start)
	if err := sc.Err(); err != nil && !errors.Is(err, io.EOF) {
		s.Err = err.Error()
	} else if s.TTFT == 0 {
		s.Err = "stream ended without a token"
	} else if s.CompletionTokens == 0 {
		s.Err = "no usage or timings in stream (server must honour stream_options.include_usage)"
	}
	return s
}

// LargeCacheHit reports a prefix-cache hit bigger than a chat-template
// header: over a quarter of the prompt served from cache. On its own that
// looks like a cached prompt, but a proxy that injects a fixed prefix (the
// router's tool proxy puts ~700 tokens of tool definitions ahead of every
// message) produces it on every request. The sweep tells the two apart by
// comparing against the warmup (see acceptCachedPrefix).
func (s Sample) LargeCacheHit() bool {
	return s.HasTimings && s.CacheN > 0 && s.CacheN*4 > s.PromptTokens
}

func deltaHasTokens(d map[string]json.RawMessage) bool {
	for _, k := range []string{"content", "reasoning_content", "reasoning", "tool_calls"} {
		v, ok := d[k]
		if !ok {
			continue
		}
		t := strings.TrimSpace(string(v))
		if t != "" && t != "null" && t != `""` && t != "[]" {
			return true
		}
	}
	return false
}

// filler is plain English so tokenisation is close to 1 token per ~4.3
// characters on every tokenizer we route to; the sweep reports the
// server's actual prompt_tokens, so this only has to be roughly right.
const filler = "The quick brown fox jumps over the lazy dog while the river runs past the old mill and the wind carries the smell of rain across the fields. "

// BuildPrompt returns a prompt of roughly targetTokens tokens that no
// prefix cache can match: a random nonce leads, so llama-server's and
// vLLM's prompt caching cannot shortcut the prefill. The instruction asks
// for a one-word reply, so prompt-processing legs spend nothing on output.
func BuildPrompt(rng *rand.Rand, targetTokens int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Session %08x-%08x. Read the passage and then reply with the single word OK.\n\n", rng.Uint32(), rng.Uint32())
	targetChars := int(float64(targetTokens) * 4.3)
	for b.Len() < targetChars {
		b.WriteString(filler)
	}
	return b.String()
}

// BuildGenPrompt returns a short, uncachable prompt that keeps a model
// generating until max_tokens cuts it off, so the generation leg measures
// steady-state decode rather than a two-token answer.
func BuildGenPrompt(rng *rand.Rand) string {
	return fmt.Sprintf("Session %08x-%08x. Write a long, detailed, multi-paragraph essay on the history of bread making, "+
		"from ancient grains to modern bakeries. Do not summarise, do not stop early, and do not ask questions; keep writing until you are cut off.",
		rng.Uint32(), rng.Uint32())
}

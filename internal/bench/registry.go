package bench

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// Registry is the subset of models.yaml a row can be enriched from. Every
// field is optional; absent data leaves the column empty for the operator.
type Registry struct {
	Models map[string]struct {
		HFRepo    string   `yaml:"hf_repo"`
		GGUFFile  string   `yaml:"gguf_file"`
		Node      string   `yaml:"node"`
		Backend   string   `yaml:"backend"`
		Aliases   []string `yaml:"aliases"`
		Context   int      `yaml:"context_length"`
		MultiNode *struct {
			Nodes    []string `yaml:"nodes"`
			HeadNode string   `yaml:"head_node"`
			TP       int      `yaml:"tensor_parallel_size"`
		} `yaml:"multi_node"`
	} `yaml:"models"`
	Nodes map[string]struct {
		GPU string `yaml:"gpu"`
	} `yaml:"nodes"`
}

// LoadRegistry parses models.yaml, ignoring everything it does not need.
func LoadRegistry(path string) (*Registry, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var r Registry
	if err := yaml.Unmarshal(b, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

var quantRe = regexp.MustCompile(`(?i)(UD-)?(IQ\d_[A-Z_]+|Q\d_[A-Z0-9_]+|BF16|F16|FP8|MXFP4|NVFP4|AWQ|GPTQ|Q\d)`)

// Enrich fills HF Repo, Quant, Host, Engine and Context from the registry
// when the row's alias (or a canonical id it aliases) is present.
func (reg *Registry) Enrich(r *Row) {
	id := r.Model
	m, ok := reg.Models[id]
	if !ok {
		for cid, cand := range reg.Models {
			for _, a := range cand.Aliases {
				if a == id {
					m, ok, id = cand, true, cid
					break
				}
			}
			if ok {
				break
			}
		}
	}
	if !ok {
		return
	}
	if r.HFRepo == "" {
		r.HFRepo = m.HFRepo
	}
	if r.Quant == "" {
		for _, src := range []string{filepath.Base(m.GGUFFile), m.HFRepo} {
			if q := quantRe.FindString(src); q != "" {
				r.Quant = q
				break
			}
		}
	}
	node := m.Node
	if node == "" && m.MultiNode != nil && len(m.MultiNode.Nodes) > 0 {
		node = strings.Join(m.MultiNode.Nodes, "+")
		if m.MultiNode.TP > 1 {
			r.Notes = strings.TrimSpace(r.Notes + fmt.Sprintf(" TP=%d across %s", m.MultiNode.TP, node))
		}
	}
	if node != "" {
		r.Host = node
		if n, ok := reg.Nodes[m.Node]; ok && r.Accelerator == "" {
			r.Accelerator = n.GPU + " (see fleet notes)"
		}
	}
	if r.Engine == "" {
		switch {
		case m.GGUFFile != "":
			r.Engine = "llama.cpp"
		case m.Backend != "":
			r.Engine = m.Backend
		}
	}
	if r.Context == nil && m.Context > 0 {
		c := m.Context
		r.Context = &c
	}
	if id != r.Model {
		r.Notes = strings.TrimSpace(r.Notes + " alias of " + id)
	}
}

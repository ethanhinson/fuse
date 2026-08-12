// Package metricspolicy provides deterministic, snapshot-based admission for
// the only unbounded Prometheus label dimensions used by Fuse.
package metricspolicy

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"sort"
)

type Dimension string

const (
	TenantID Dimension = "tenant_id"
	Model    Dimension = "model"
	Tool     Dimension = "tool"
	Overflow           = "__overflow__"
	// HashV1 hashes length-prefixed salt, dimension and value with SHA-256 and
	// orders candidates by the first 64 bits (big endian), then by value.
	HashV1 = "sha256-64-v1"
)

type DimensionConfig struct {
	Budget          int
	Catalog, Pinned []string
}
type Config struct {
	HashVersion, Salt string
	Dimensions        map[Dimension]DimensionConfig
}
type Policy struct {
	budgets  map[Dimension]int
	admitted map[Dimension]map[string]struct{}
}

func New(cfg Config) (*Policy, error) {
	if cfg.HashVersion != HashV1 {
		return nil, fmt.Errorf("unsupported cardinality hash version %q", cfg.HashVersion)
	}
	p := &Policy{budgets: map[Dimension]int{}, admitted: map[Dimension]map[string]struct{}{}}
	for _, dimension := range []Dimension{TenantID, Model, Tool} {
		dc := cfg.Dimensions[dimension]
		if dc.Budget < 0 {
			return nil, fmt.Errorf("%s budget must be nonnegative", dimension)
		}
		if len(unique(dc.Pinned)) > dc.Budget {
			return nil, fmt.Errorf("%s pins exceed budget", dimension)
		}
		catalog := unique(dc.Catalog)
		pins := unique(dc.Pinned)
		available := make(map[string]struct{}, len(catalog))
		for _, value := range catalog {
			if value == Overflow {
				return nil, fmt.Errorf("%s is reserved", Overflow)
			}
			available[value] = struct{}{}
		}
		selected := map[string]struct{}{}
		for _, value := range pins {
			if value == Overflow {
				return nil, fmt.Errorf("%s is reserved", Overflow)
			}
			if _, ok := available[value]; !ok {
				return nil, fmt.Errorf("pinned %s %q absent from catalog", dimension, value)
			}
			selected[value] = struct{}{}
		}
		type candidate struct {
			value string
			score uint64
		}
		candidates := make([]candidate, 0, len(catalog))
		for _, value := range catalog {
			if _, pinned := selected[value]; !pinned {
				candidates = append(candidates, candidate{value, stableHash(cfg.Salt, dimension, value)})
			}
		}
		sort.Slice(candidates, func(i, j int) bool {
			if candidates[i].score == candidates[j].score {
				return candidates[i].value < candidates[j].value
			}
			return candidates[i].score < candidates[j].score
		})
		for _, c := range candidates {
			if len(selected) >= dc.Budget {
				break
			}
			selected[c.value] = struct{}{}
		}
		p.budgets[dimension], p.admitted[dimension] = dc.Budget, selected
	}
	return p, nil
}

func (p *Policy) Map(d Dimension, value string) string {
	if p != nil {
		if _, ok := p.admitted[d][value]; ok {
			return value
		}
	}
	return Overflow
}
func (p *Policy) Budget(d Dimension) int {
	if p == nil {
		return 0
	}
	return p.budgets[d]
}
func (p *Policy) AdmittedCount(d Dimension) int {
	if p == nil {
		return 0
	}
	return len(p.admitted[d])
}
func (p *Policy) Admitted(d Dimension) []string {
	var out []string
	if p != nil {
		for v := range p.admitted[d] {
			out = append(out, v)
		}
	}
	sort.Strings(out)
	return out
}

func unique(values []string) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(values))
	for _, v := range values {
		if _, ok := seen[v]; !ok {
			seen[v] = struct{}{}
			out = append(out, v)
		}
	}
	return out
}
func stableHash(salt string, d Dimension, value string) uint64 {
	h := sha256.New()
	for _, part := range []string{salt, string(d), value} {
		var b [4]byte
		binary.BigEndian.PutUint32(b[:], uint32(len(part)))
		h.Write(b[:])
		h.Write([]byte(part))
	}
	return binary.BigEndian.Uint64(h.Sum(nil)[:8])
}

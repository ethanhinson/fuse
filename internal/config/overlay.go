package config

import "maps"

// clone returns a copy of c whose maps the loader's merge writes into
// (Models.Entries, Workflows and each workflow's Workers) are its own, so
// OverlaySpec never mutates the operator's config. Slices and the other maps
// are only ever replaced by the merge, never written through, so sharing them
// is safe.
func (c Config) clone() Config {
	out := c
	out.Models.Entries = maps.Clone(c.Models.Entries)
	if c.Workflows != nil {
		out.Workflows = make(map[string]WorkflowConfig, len(c.Workflows))
		for name, w := range c.Workflows {
			w.Workers = maps.Clone(w.Workers)
			out.Workflows[name] = w
		}
	}
	return out
}

// mergeSpecPermissions merges a harness spec's permissions onto c (change
// 0085). The spec's permission posture is client-settable (ADR-0060), so mode,
// session_allow, auto_approve and the auto-mode classifier, deny, ask and
// allow_push keys are honored field by field where set. The ADR-0006
// tightening lists are the operator's floor: the spec's entries are added to
// them, never substituted for them. write_roots is operator-only and was
// already dropped by the caller.
func mergeSpecPermissions(c *Config, raw rawPermissionsConfig) {
	if raw.Mode != "" {
		c.Permissions.Mode = raw.Mode
	}
	if raw.SessionAllow != nil {
		c.Permissions.SessionAllow = *raw.SessionAllow
	}
	if len(raw.AutoApprove) > 0 {
		c.Permissions.AutoApprove = raw.AutoApprove
	}
	c.Permissions.AlwaysPrompt = union(c.Permissions.AlwaysPrompt, raw.AlwaysPrompt)
	c.Permissions.Disabled = union(c.Permissions.Disabled, raw.Disabled)

	a := raw.Auto
	if a.ClassifierModel != "" {
		c.Permissions.Auto.ClassifierModel = a.ClassifierModel
	}
	if len(a.Deny) > 0 {
		c.Permissions.Auto.Deny = a.Deny
	}
	if len(a.Ask) > 0 {
		c.Permissions.Auto.Ask = a.Ask
	}
	if a.AllowPush {
		c.Permissions.Auto.AllowPush = true
	}
	c.Permissions.Auto.FetchDeny = union(c.Permissions.Auto.FetchDeny, a.FetchDeny)
	c.Permissions.Auto.FetchAsk = union(c.Permissions.Auto.FetchAsk, a.FetchAsk)
}

// union returns floor followed by every entry of add not already present, as
// a new slice (floor is never written through). With nothing to add it
// returns floor unchanged.
func union(floor, add []string) []string {
	if len(add) == 0 {
		return floor
	}
	seen := make(map[string]bool, len(floor)+len(add))
	out := make([]string, 0, len(floor)+len(add))
	for _, s := range floor {
		seen[s] = true
		out = append(out, s)
	}
	for _, s := range add {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// mergeSpecWorkflows merges a harness spec's workflows onto c (change 0085). A
// workflow the operator does not define belongs to the client and is taken
// whole. For an operator-defined workflow the skill and workers are
// client-settable and taken where set, while each pool number is tighten-only
// under the same rule as every other ADR-0006 axis (tightenNumeric, untrusted):
// a refused loosening keeps the operator's value and is returned as
// "workflows.<name>.pool.<field>".
func mergeSpecWorkflows(c *Config, raw map[string]WorkflowConfig) (refused []string) {
	if len(raw) == 0 {
		return nil
	}
	if c.Workflows == nil {
		c.Workflows = map[string]WorkflowConfig{}
	}
	for name, rw := range raw {
		cur, exists := c.Workflows[name]
		if !exists {
			c.Workflows[name] = rw
			continue
		}
		if rw.Skill != "" {
			cur.Skill = rw.Skill
		}
		if len(rw.Workers) > 0 {
			cur.Workers = rw.Workers
		}
		pool := func(field string, curV *int, newV int) {
			if tightenNumeric(curV, newV, false) {
				refused = append(refused, "workflows."+name+".pool."+field)
			}
		}
		pool("concurrent", &cur.Pool.Concurrent, rw.Pool.Concurrent)
		pool("total", &cur.Pool.Total, rw.Pool.Total)
		pool("max_depth", &cur.Pool.MaxDepth, rw.Pool.MaxDepth)
		pool("tokens", &cur.Pool.Tokens, rw.Pool.Tokens)
		c.Workflows[name] = cur
	}
	return refused
}

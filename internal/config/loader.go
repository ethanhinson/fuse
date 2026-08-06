package config

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// warnw is where startup trust-boundary warnings are written. It defaults to
// stderr and is swappable in tests.
var warnw io.Writer = os.Stderr

// Load resolves configuration by starting from Default(), merging
// ~/.fuse/config.yml if present, then .fuse.local.yml in the CWD if present,
// and finally applying LLM_GATEWAY_URL / LLM_GATEWAY_KEY env overrides.
func Load() (Config, error) {
	c := Default()

	var projects map[string]ProjectConfig
	home, err := os.UserHomeDir()
	if err == nil {
		// The trusted home file is the only source whose projects: map is
		// captured (via the sink) and later applied.
		if err := mergeFile(&c, filepath.Join(home, ".fuse", "config.yml"), true, &projects); err != nil {
			return c, err
		}
	}
	// Per-project overrides sit above the global permissions and below the
	// tighten-only .fuse.local.yml: a matching, user-owned (trusted) entry
	// resolves into c.Permissions for the current cwd.
	applyProjectOverride(&c, projects)
	// .fuse.local.yml is repo-plantable (untrusted): permission-loosening keys
	// (including any projects: block) are ignored so a checked-in file cannot
	// weaken the gate. It passes nil for the projects sink.
	if err := mergeFile(&c, ".fuse.local.yml", false, nil); err != nil {
		return c, err
	}

	if v := os.Getenv("LLM_GATEWAY_URL"); v != "" {
		c.Gateway.URL = v
	}
	if v := os.Getenv("LLM_GATEWAY_KEY"); v != "" {
		c.Gateway.Key = v
	}
	return c, nil
}

// mergePermissions merges a raw permissions subtree onto c following the
// trust-boundary rules: loosening keys (mode/session_allow/auto_approve/auto.*)
// are honored only when trusted, otherwise each present one is collected into
// the returned ignored slice; tightening keys (always_prompt/disabled) are
// honored from either source. It emits no warning itself — the caller renders
// the single aggregated warning line from the returned slice.
func mergePermissions(c *Config, raw rawPermissionsConfig, trusted bool) (ignored []string) {
	if raw.Mode != "" {
		if trusted {
			c.Permissions.Mode = raw.Mode
		} else {
			ignored = append(ignored, "permissions.mode")
		}
	}
	// session_allow (a *bool) is only a loosening override when explicitly set.
	if raw.SessionAllow != nil {
		if trusted {
			c.Permissions.SessionAllow = *raw.SessionAllow
		} else {
			ignored = append(ignored, "permissions.session_allow")
		}
	}
	if len(raw.AutoApprove) > 0 {
		if trusted {
			c.Permissions.AutoApprove = raw.AutoApprove
		} else {
			ignored = append(ignored, "permissions.auto_approve")
		}
	}
	// Tightening keys stay honored from either source.
	if len(raw.AlwaysPrompt) > 0 {
		c.Permissions.AlwaysPrompt = raw.AlwaysPrompt
	}
	if len(raw.Disabled) > 0 {
		c.Permissions.Disabled = raw.Disabled
	}
	// The whole permissions.auto.* block is loosening surface.
	if raw.Auto.ClassifierModel != "" || len(raw.Auto.Deny) > 0 || len(raw.Auto.Ask) > 0 {
		if trusted {
			c.Permissions.Auto = raw.Auto
		} else {
			ignored = append(ignored, "permissions.auto")
		}
	}
	return ignored
}

// mergeWorkflows merges a source's workflows: map into c. A config-level entry
// overrides any prior one for the same name (trusted sources set every present
// field). From an UNTRUSTED source (.fuse.local.yml) pool numbers may only
// TIGHTEN — lower an existing, already-set cap — because a workflow pool is a
// safety brake and a repo-plantable file must not be able to widen it. A present
// pool number that would loosen an existing cap is dropped and its workflow name
// is returned for the caller's aggregated warning. Non-pool fields (skill,
// workers) from an untrusted source are also inert (they could re-grant tools);
// the workflow definition itself is human-owned config.
//
// "Tighten" is defined per dimension against the current value: a value is a
// tightening only when the current value is already set (>0) and the new value
// is a positive number strictly lower than it. Everything else from an untrusted
// source — introducing a new workflow, setting a previously-unset dimension, or
// raising a cap — is a loosening and ignored.
func mergeWorkflows(c *Config, raw map[string]WorkflowConfig, trusted bool) (loosened []string) {
	if len(raw) == 0 {
		return nil
	}
	if c.Workflows == nil {
		c.Workflows = map[string]WorkflowConfig{}
	}
	for name, rw := range raw {
		if trusted {
			cur := c.Workflows[name]
			if rw.Skill != "" {
				cur.Skill = rw.Skill
			}
			if rw.Pool.Concurrent != 0 {
				cur.Pool.Concurrent = rw.Pool.Concurrent
			}
			if rw.Pool.Total != 0 {
				cur.Pool.Total = rw.Pool.Total
			}
			if rw.Pool.MaxDepth != 0 {
				cur.Pool.MaxDepth = rw.Pool.MaxDepth
			}
			if len(rw.Workers) > 0 {
				cur.Workers = rw.Workers
			}
			c.Workflows[name] = cur
			continue
		}
		// Untrusted: only tightening pool numbers on an EXISTING workflow apply.
		cur, exists := c.Workflows[name]
		if !exists {
			loosened = append(loosened, name)
			continue
		}
		flagged := false
		tightenOnly := func(field string, curV, newV *int) {
			if *newV == 0 {
				return // omitted dimension
			}
			if *curV > 0 && *newV > 0 && *newV < *curV {
				*curV = *newV // strictly-lower positive = tighten, honored
				return
			}
			if !flagged {
				loosened = append(loosened, name)
				flagged = true
			}
		}
		tightenOnly("concurrent", &cur.Pool.Concurrent, &rw.Pool.Concurrent)
		tightenOnly("total", &cur.Pool.Total, &rw.Pool.Total)
		tightenOnly("max_depth", &cur.Pool.MaxDepth, &rw.Pool.MaxDepth)
		// skill / workers changes from an untrusted file are loosening surface.
		if rw.Skill != "" && rw.Skill != cur.Skill || len(rw.Workers) > 0 {
			if !flagged {
				loosened = append(loosened, name)
			}
		}
		c.Workflows[name] = cur
	}
	return loosened
}

// applyProjectOverride merges the per-project permissions entry whose key best
// matches the current cwd. The winning key is the LONGEST project key that
// equals cwd or is a path-segment ancestor of it. Keys and cwd are canonicalized
// via filepath.EvalSymlinks; if cwd cannot be resolved the whole step is a
// no-op, and any key that fails to resolve is skipped. The matched entry merges
// as trusted (full subtree incl. mode and auto.*). No match ⇒ no-op.
func applyProjectOverride(c *Config, projects map[string]ProjectConfig) {
	if len(projects) == 0 {
		return
	}
	cwd, err := os.Getwd()
	if err != nil {
		return
	}
	cwd, err = filepath.EvalSymlinks(cwd)
	if err != nil {
		return
	}
	cwd = filepath.Clean(cwd)

	var bestLen int // length of the winning key; longest match wins
	var best ProjectConfig
	found := false
	for key, entry := range projects {
		canon, err := filepath.EvalSymlinks(key)
		if err != nil {
			continue // a key that does not resolve is skipped, not fatal
		}
		canon = filepath.Clean(canon)
		// Path-segment ancestor test: never a raw HasPrefix (that would wrongly
		// match /a/bc under a key /a/b).
		if cwd != canon && !strings.HasPrefix(cwd, canon+string(os.PathSeparator)) {
			continue
		}
		if !found || len(canon) > bestLen {
			bestLen = len(canon)
			best = entry
			found = true
		}
	}
	if !found {
		return
	}
	mergePermissions(c, best.Permissions, true)
}

// mergeFile applies a YAML file onto c if the file exists. A missing file is
// not an error; a malformed file is. When trusted is false the source is
// repo-plantable (e.g. .fuse.local.yml): permission-LOOSENING keys are ignored
// with an aggregated startup warning, while tightening keys stay honored. The
// projects sink, when non-nil, captures the parsed projects: map (only the
// trusted home call passes a sink; the untrusted local call passes nil).
func mergeFile(c *Config, path string, trusted bool, projects *map[string]ProjectConfig) error {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read config %s: %w", path, err)
	}

	var raw rawConfig
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("parse config %s: %w", path, err)
	}

	if raw.Gateway.URL != "" {
		c.Gateway.URL = raw.Gateway.URL
	}
	if raw.Gateway.Key != "" {
		c.Gateway.Key = raw.Gateway.Key
	}
	// A present max_turns (including an explicit 0) overrides; an omitted key
	// (nil) leaves the unset default for the call site to resolve. (0038)
	if raw.MaxTurns != nil {
		c.MaxTurns = raw.MaxTurns
	}
	if raw.MaxTokens != 0 {
		c.MaxTokens = raw.MaxTokens
	}
	if len(raw.SkillPaths) > 0 {
		c.SkillPaths = raw.SkillPaths
	}
	// Permission-loosening keys are honored only from a trusted source. From an
	// untrusted (repo-plantable) source they are inert and each present one is
	// collected for a single aggregated startup warning.
	ignored := mergePermissions(c, raw.Permissions, trusted)
	// Capture the trusted home file's projects: map for applyProjectOverride.
	if projects != nil {
		*projects = raw.Projects
	}
	// A projects: block in an untrusted file is a loosening surface (a repo file
	// could grant itself mode: auto): ignore it and name it in the aggregated
	// warning. Only the trusted home path ever applies raw.Projects.
	if !trusted && len(raw.Projects) > 0 {
		ignored = append(ignored, "projects")
	}
	if !trusted && len(ignored) > 0 {
		fmt.Fprintf(warnw, "warning: %s ignores permission-loosening keys (%s); set these in ~/.fuse/config.yml instead\n",
			path, strings.Join(ignored, ", "))
	}
	if len(raw.MCPServers) > 0 {
		c.MCPServers = raw.MCPServers
	}

	if raw.Research.Provider != "" {
		c.Research.Provider = raw.Research.Provider
	}
	if raw.Research.MaxQueries != 0 {
		c.Research.MaxQueries = raw.Research.MaxQueries
	}
	if raw.Research.MaxResults != 0 {
		c.Research.MaxResults = raw.Research.MaxResults
	}
	if raw.Research.MaxContentKB != 0 {
		c.Research.MaxContentKB = raw.Research.MaxContentKB
	}
	// RespectRobots defaults true, so a plain zero-check cannot tell "unset"
	// from "false" when a research block is present; a pointer lets an omitted
	// key keep the default while `respect_robots: false` still takes effect.
	if raw.Research.RespectRobots != nil {
		c.Research.RespectRobots = *raw.Research.RespectRobots
	}
	if raw.Research.Custom.URL != "" {
		c.Research.Custom.URL = raw.Research.Custom.URL
	}
	if len(raw.Research.Custom.Headers) > 0 {
		c.Research.Custom.Headers = raw.Research.Custom.Headers
	}
	if raw.Research.Custom.ResultsPath != "" {
		c.Research.Custom.ResultsPath = raw.Research.Custom.ResultsPath
	}
	if raw.Research.Custom.TitleField != "" {
		c.Research.Custom.TitleField = raw.Research.Custom.TitleField
	}
	if raw.Research.Custom.URLField != "" {
		c.Research.Custom.URLField = raw.Research.Custom.URLField
	}
	if raw.Research.Custom.SnippetField != "" {
		c.Research.Custom.SnippetField = raw.Research.Custom.SnippetField
	}

	// Agents budget: a nonzero override replaces the default; an omitted key
	// (zero) keeps the built-in 64. A negative override is nonsensical for a
	// budget and would silently DISABLE the strip predicate's budget brake
	// (which guards on max > 0), so it is clamped back to the default rather
	// than propagated raw.
	if raw.Agents.MaxSpawns != 0 {
		c.Agents.MaxSpawns = raw.Agents.MaxSpawns
	}
	if c.Agents.MaxSpawns < 0 {
		c.Agents.MaxSpawns = 64
	}
	// Agents concurrency: a nonzero override replaces the default; an omitted
	// key (zero) keeps the built-in 16. A negative override is clamped to the
	// default for the same reason as MaxSpawns: the scheduler guards its slot
	// brake on a positive cap, so a raw negative would silently disable the
	// active-child queueing even though NewAgentTreeWithConcurrency clamps its
	// own copy. Clamping once here keeps the tree's semaphore and
	// the predicate's cap consistent.
	if raw.Agents.MaxConcurrent != 0 {
		c.Agents.MaxConcurrent = raw.Agents.MaxConcurrent
	}
	if c.Agents.MaxConcurrent < 0 {
		c.Agents.MaxConcurrent = 16
	}

	// Workflows: a config-level entry overrides any prior one for the same name.
	// From an untrusted source (.fuse.local.yml) pool numbers may only TIGHTEN
	// (lower an existing cap); a loosening value is ignored and the workflow is
	// named in the aggregated warning (ADR-0006 trust boundary).
	if loosened := mergeWorkflows(c, raw.Workflows, trusted); !trusted && len(loosened) > 0 {
		fmt.Fprintf(warnw, "warning: %s ignores workflow pool-loosening keys (%s); set these in ~/.fuse/config.yml instead\n",
			path, strings.Join(loosened, ", "))
	}

	// The `models` map holds a `default` string alongside model entries.
	for k, v := range raw.Models {
		if k == "default" {
			if s, ok := v.(string); ok {
				c.Models.Default = s
			}
			continue
		}
		mc, err := decodeModelEntry(v)
		if err != nil {
			return fmt.Errorf("parse model %q in %s: %w", k, path, err)
		}
		if c.Models.Entries == nil {
			c.Models.Entries = map[string]ModelConfig{}
		}
		c.Models.Entries[k] = mc
	}
	return nil
}

// decodeModelEntry converts a single YAML model node into a ModelConfig.
func decodeModelEntry(v interface{}) (ModelConfig, error) {
	// Round-trip through YAML to reuse struct tags without a custom decoder.
	data, err := yaml.Marshal(v)
	if err != nil {
		return ModelConfig{}, err
	}
	var mc ModelConfig
	if err := yaml.Unmarshal(data, &mc); err != nil {
		return ModelConfig{}, err
	}
	return mc, nil
}

package sandbox

import (
	"os"
	"strings"
)

// baseAllowlist is the fixed set of host variables every sandboxed command may
// observe. It is deliberately tiny: enough for a shell to find binaries, a home
// directory, and a locale, and nothing else. Anything beyond this is an
// operator decision, made explicitly via env_passthrough.
var baseAllowlist = [...]string{"PATH", "HOME", "LANG"}

// ResolveEnv builds the complete environment a sandboxed command may observe.
//
// The result is the base allowlist plus each operator-configured passthrough
// key, each read through lookup. EVERY other host variable is dropped: there is
// no inheritance path, on any substrate. A key that lookup does not report as
// present is silently omitted rather than being materialized as an empty-string
// entry, so an unset passthrough cannot mask a variable the command would
// otherwise legitimately see, and cannot fabricate a bogus value.
//
// lookup is injected (see ResolveEnvFromOS for the process-environment default)
// so callers and tests can resolve against a fixed environment without mutating
// the real one.
//
// The returned Env.Allow is always non-nil, including when nothing resolves: a
// nil environment means "inherit" to os/exec, which is the exact failure mode
// this resolver exists to make unrepresentable.
func ResolveEnv(passthrough []string, lookup func(string) (string, bool)) Env {
	allow := make(map[string]string, len(baseAllowlist)+len(passthrough))
	if lookup == nil {
		return Env{Allow: allow}
	}

	add := func(key string) {
		key = strings.TrimSpace(key)
		if key == "" {
			return
		}
		if v, ok := lookup(key); ok {
			allow[key] = v
		}
	}

	for _, key := range baseAllowlist {
		add(key)
	}
	for _, key := range passthrough {
		add(key)
	}

	return Env{Allow: allow}
}

// ResolveEnvFromOS is ResolveEnv against the real process environment. It is
// the default used at the composition root; tests should prefer ResolveEnv with
// an explicit lookup.
func ResolveEnvFromOS(passthrough []string) Env {
	return ResolveEnv(passthrough, os.LookupEnv)
}

package harnessresolve

import (
	"context"
	"sort"

	harnessv1 "github.com/ethanhinson/fuse/harness/v1"
	"github.com/ethanhinson/fuse/internal/config"
	"github.com/ethanhinson/fuse/internal/harnessconfig"
)

// specAuthAllowed is the single predicate behind operatorOnlyProblems (the
// refusal) and SpecMCPAuthKinds (the advertisement). An enum value with no
// config auth string is not allowed.
func specAuthAllowed(t harnessv1.McpAuthType) bool {
	s, ok := harnessconfig.AuthTypeToConfig(t)
	return ok && config.SpecMCPAuthTypes[s]
}

// SpecMCPAuthKinds lists, in enum order, the MCP auth types a spec may use.
// UNSPECIFIED is listed: it means none.
func SpecMCPAuthKinds() []harnessv1.McpAuthType {
	vals := harnessv1.McpAuthType(0).Descriptor().Values()
	var out []harnessv1.McpAuthType
	for i := 0; i < vals.Len(); i++ {
		if t := harnessv1.McpAuthType(vals.Get(i).Number()); specAuthAllowed(t) {
			out = append(out, t)
		}
	}
	return out
}

// SecretLister is a SecretResolver that can list the names it would resolve
// for a tenant: names only, never values or sources.
type SecretLister interface {
	SecretNames(ctx context.Context, tenant string) []string
}

// SecretRefs lists, sorted, the secret_ref names that resolve for tenant. A
// resolver that cannot list (or none) lists nothing: deny by default. A name
// restricted to hosts is listed; StartLoop still sends its value only to a
// destination on that list.
func (r Resolver) SecretRefs(ctx context.Context, tenant string) []string {
	l, ok := r.Secrets.(SecretLister)
	if !ok {
		return nil
	}
	names := l.SecretNames(ctx, tenant)
	sort.Strings(names)
	return names
}

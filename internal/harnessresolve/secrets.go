package harnessresolve

import (
	"context"
	"errors"
	"net/url"
	"os"
	"slices"
	"strings"

	"github.com/ethanhinson/fuse/internal/config"
	"github.com/ethanhinson/fuse/internal/event"
	"github.com/ethanhinson/fuse/internal/netguard"
)

// ErrSecretUnavailable is the one error a SecretResolver returns for a name it
// will not resolve for a tenant, whatever the reason (unknown name, tenant not
// allowed, source missing or empty). It never carries the value or the source.
var ErrSecretUnavailable = errors.New("harnessresolve: secret not available")

// SecretResolver turns a secret_ref NAME into its value for a loop of tenant
// (change 0085). destination is the URL the value will be sent to (the MCP
// server's url, or research.custom.url), which the spec itself chooses, so a
// resolver that hands a value to any destination discloses it to the tenant.
// Implementations must return ErrSecretUnavailable (possibly wrapped) for every
// refusal and must never put a value in an error.
type SecretResolver interface {
	ResolveSecret(ctx context.Context, tenant, name, destination string) (string, error)
}

// ConfigSecrets is the operator-configured SecretResolver: an explicit
// allowlist (loop_server.secrets) mapping each name to an environment variable
// or a file on the server host, to the tenants that may reference it, and
// optionally to the hostnames it may be sent to. An entry with no tenants is
// available to the default tenant only; an entry with hosts is refused for any
// destination whose hostname is not listed. A name not in the list is refused,
// so an empty list denies everything.
func ConfigSecrets(entries []config.SecretSourceConfig) SecretResolver {
	byName := make(map[string]config.SecretSourceConfig, len(entries))
	for _, e := range entries {
		byName[e.Name] = e
	}
	return configSecrets{byName: byName}
}

type configSecrets struct {
	byName map[string]config.SecretSourceConfig
}

func (s configSecrets) ResolveSecret(_ context.Context, tenant, name, destination string) (string, error) {
	e, ok := s.byName[name]
	if !ok {
		return "", ErrSecretUnavailable
	}
	v, ok := available(e, tenant)
	if !ok {
		return "", ErrSecretUnavailable
	}
	if len(e.Hosts) > 0 && !destinationAllowed(e.Hosts, destination) {
		return "", ErrSecretUnavailable
	}
	return v, nil
}

// SecretNames lists the names available to tenant, unsorted: names only,
// never values or sources.
func (s configSecrets) SecretNames(_ context.Context, tenant string) []string {
	var out []string
	for name, e := range s.byName {
		if _, ok := available(e, tenant); ok {
			out = append(out, name)
		}
	}
	return out
}

// available is the tenant rule and source read every resolution applies.
// ResolveSecret adds the destination-host rule on top; SecretNames lists by
// this rule alone, so a host-restricted name is listed but still resolves only
// for a destination on its hosts list.
func available(e config.SecretSourceConfig, tenant string) (string, bool) {
	t := string(event.NormalizeTenant(event.TenantID(tenant)))
	allowed := e.Tenants
	if len(allowed) == 0 {
		allowed = []string{string(event.DefaultTenant)}
	}
	if !slices.Contains(allowed, t) {
		return "", false
	}
	var v string
	if e.Env != "" {
		v = os.Getenv(e.Env)
	} else {
		b, err := os.ReadFile(e.File)
		if err != nil {
			return "", false
		}
		v = strings.TrimSuffix(strings.TrimSuffix(string(b), "\n"), "\r")
	}
	return v, v != ""
}

// canonicalHost is the one spelling every host rule compares: lower case, no
// trailing root dot. The operator secret hosts rule, the loop-scoped secret
// hosts rule and the spec policy all match through it, so no two layers can
// disagree on a spelling.
func canonicalHost(h string) string { return netguard.CanonicalHost(h) }

// destinationAllowed reports whether destination's hostname is one of hosts.
// An unparsable destination, an empty hostname or an empty hosts list is never
// allowed.
func destinationAllowed(hosts []string, destination string) bool {
	u, err := url.Parse(destination)
	if err != nil {
		return false
	}
	host := canonicalHost(u.Hostname())
	if host == "" {
		return false
	}
	return slices.ContainsFunc(hosts, func(h string) bool { return canonicalHost(h) == host })
}

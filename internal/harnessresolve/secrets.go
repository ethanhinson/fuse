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
	t := string(event.NormalizeTenant(event.TenantID(tenant)))
	allowed := e.Tenants
	if len(allowed) == 0 {
		allowed = []string{string(event.DefaultTenant)}
	}
	if !slices.Contains(allowed, t) {
		return "", ErrSecretUnavailable
	}
	if len(e.Hosts) > 0 {
		u, err := url.Parse(destination)
		if err != nil || !slices.ContainsFunc(e.Hosts, func(h string) bool { return strings.EqualFold(h, u.Hostname()) }) {
			return "", ErrSecretUnavailable
		}
	}
	var v string
	if e.Env != "" {
		v = os.Getenv(e.Env)
	} else {
		b, err := os.ReadFile(e.File)
		if err != nil {
			return "", ErrSecretUnavailable
		}
		v = strings.TrimSuffix(strings.TrimSuffix(string(b), "\n"), "\r")
	}
	if v == "" {
		return "", ErrSecretUnavailable
	}
	return v, nil
}

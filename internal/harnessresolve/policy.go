package harnessresolve

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ethanhinson/fuse/harness"
	harnessv1 "github.com/ethanhinson/fuse/harness/v1"
	"github.com/ethanhinson/fuse/internal/config"
	"github.com/ethanhinson/fuse/internal/harnessconfig"
)

// resolveTimeout bounds the one hostname resolution deny_private_networks does
// per URL (change 0101).
const resolveTimeout = 2 * time.Second

// lookupIP resolves a hostname for the private-network check. A variable so
// tests are hermetic.
var lookupIP = func(ctx context.Context, host string) ([]net.IP, error) {
	addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, err
	}
	out := make([]net.IP, len(addrs))
	for i, a := range addrs {
		out[i] = a.IP
	}
	return out, nil
}

// specPolicyProblems applies loop_server.spec_policy (change 0101) to the
// spec's own egress URLs and model ids. Operator-configured MCP servers are
// not in the spec, so they are never checked. Every violation is
// RuleOperatorOnly at the spec field path.
func (r Resolver) specPolicyProblems(ctx context.Context, spec *harnessv1.HarnessSpec) []harnessconfig.Problem {
	pol := r.Base.LoopServer.SpecPolicy
	var out []harnessconfig.Problem
	for i, s := range spec.GetMcpServers() {
		if msg := checkURL(ctx, pol, s.GetUrl()); msg != "" {
			out = append(out, policyProblem(fmt.Sprintf("mcp_servers[%d].url", i), msg))
		}
	}
	if u := spec.GetResearch().GetCustom().GetUrl(); u != "" {
		if msg := checkURL(ctx, pol, u); msg != "" {
			out = append(out, policyProblem("research.custom.url", msg))
		}
	}
	if pol.RegistryOnly() {
		allowed := registryGatewayIDs(r.Base)
		aliases := make([]string, 0, len(spec.GetModels().GetEntries()))
		for a := range spec.GetModels().GetEntries() {
			aliases = append(aliases, a)
		}
		sort.Strings(aliases)
		for _, a := range aliases {
			id := spec.GetModels().GetEntries()[a].GetId()
			if !allowed[id] {
				out = append(out, policyProblem(fmt.Sprintf("models.entries[%s].id", strconv.Quote(a)),
					"this server only runs its own registry models; "+strconv.Quote(id)+" is not one of them"))
			}
		}
	}
	return out
}

func policyProblem(path, msg string) harnessconfig.Problem {
	return harnessconfig.Problem{FieldPath: path, Message: msg, RuleID: harness.RuleOperatorOnly}
}

// registryGatewayIDs is the set of gateway ids of the advertised operator
// registry entries: the same set GetCapabilities lists.
func registryGatewayIDs(cfg config.Config) map[string]bool {
	out := map[string]bool{}
	for _, e := range cfg.Models.Entries {
		if e.Advertise == nil || *e.Advertise {
			out[e.ID] = true
		}
	}
	return out
}

// checkURL returns the refusal for raw under pol, or "". Messages never echo
// resolved addresses, so a client cannot map the server's network.
func checkURL(ctx context.Context, pol config.SpecPolicyConfig, raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" {
		return "is not an absolute URL with a host"
	}
	host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	switch u.Scheme {
	case "https":
	case "http":
		if !hostMatchesAny(host, pol.AllowHTTPHosts) {
			return "must use https on this server"
		}
	default:
		return "must use https on this server"
	}
	if len(pol.MCPHosts) > 0 && !hostMatchesAny(host, pol.MCPHosts) {
		return "host is not allowed by this server"
	}
	if pol.DenyPrivate() {
		if privateName(host) {
			return "host is on a private network, which this server does not allow"
		}
		if ip := net.ParseIP(host); ip != nil {
			if privateIP(ip) {
				return "host is on a private network, which this server does not allow"
			}
			return ""
		}
		rctx, cancel := context.WithTimeout(ctx, resolveTimeout)
		defer cancel()
		ips, err := lookupIP(rctx, host)
		if err != nil || len(ips) == 0 {
			return "host could not be resolved"
		}
		for _, ip := range ips {
			if privateIP(ip) {
				return "host is on a private network, which this server does not allow"
			}
		}
	}
	return ""
}

// hostMatchesAny reports whether host equals a pattern or, for "*.suffix",
// is a subdomain of suffix. Patterns match case-insensitively.
func hostMatchesAny(host string, patterns []string) bool {
	for _, p := range patterns {
		p = strings.ToLower(strings.TrimSuffix(p, "."))
		if suffix, ok := strings.CutPrefix(p, "*."); ok {
			if strings.HasSuffix(host, "."+suffix) {
				return true
			}
			continue
		}
		if host == p {
			return true
		}
	}
	return false
}

func privateName(host string) bool {
	return host == "localhost" || strings.HasSuffix(host, ".localhost") ||
		strings.HasSuffix(host, ".local") || strings.HasSuffix(host, ".internal")
}

func privateIP(ip net.IP) bool {
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsUnspecified() || ip.IsInterfaceLocalMulticast()
}

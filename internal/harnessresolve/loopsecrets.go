package harnessresolve

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"unicode"

	"github.com/ethanhinson/fuse/harness"
	"github.com/ethanhinson/fuse/internal/config"
	"github.com/ethanhinson/fuse/internal/harnessconfig"
	"github.com/ethanhinson/fuse/internal/loopsecret"
)

// secretNameRE is the shape of a loop secret's name: the secret_ref pattern a
// spec's references and the operator's entries already follow.
var secretNameRE = regexp.MustCompile(config.SecretRefPattern)

// loopSecrets indexes the request's loop-scoped secrets (change 0102) by name
// and reports every invalid entry at its request path (secrets[i].name, .value,
// .hosts, .hosts[j]). An invalid entry is left out of the index, so it never
// resolves anything; a later entry repeating a name is invalid too, so it can
// never stand in for an earlier entry of that name. No message carries a value.
func loopSecrets(ss []loopsecret.Secret) (map[string]loopsecret.Secret, []harnessconfig.Problem) {
	index := map[string]loopsecret.Secret{}
	seen := map[string]bool{}
	var problems []harnessconfig.Problem
	add := func(path, msg string) {
		problems = append(problems, harnessconfig.Problem{FieldPath: path, Message: msg, RuleID: harness.RuleSecretRef})
	}
	for i, s := range ss {
		valid := true
		switch {
		case !secretNameRE.MatchString(s.Name):
			add(fmt.Sprintf("secrets[%d].name", i), "must match "+config.SecretRefPattern)
			valid = false
		case seen[s.Name]:
			add(fmt.Sprintf("secrets[%d].name", i), "duplicates the name of an earlier loop secret")
			valid = false
		}
		seen[s.Name] = true
		if s.Value == "" {
			add(fmt.Sprintf("secrets[%d].value", i), "must not be empty")
			valid = false
		}
		if len(s.Hosts) == 0 {
			add(fmt.Sprintf("secrets[%d].hosts", i), "a loop secret must list the hosts its value may be sent to")
			valid = false
		}
		for j, h := range s.Hosts {
			switch {
			case h == "":
				add(fmt.Sprintf("secrets[%d].hosts[%d]", i, j), "must not be empty")
				valid = false
			case strings.ContainsAny(h, ":/*") || strings.ContainsFunc(h, unicode.IsSpace):
				// A scheme, port, path, wildcard or space never matches a
				// destination's hostname, so the entry would silently never
				// resolve; refuse it, as config does for operator hosts.
				add(fmt.Sprintf("secrets[%d].hosts[%d]", i, j), "must be a bare hostname (no scheme, port, path, wildcard or whitespace)")
				valid = false
			}
		}
		if valid {
			index[s.Name] = s
		}
	}
	return index, problems
}

// loopHostProblem is the violation for a loop secret whose hosts do not list
// the reference's destination. It names the destination's canonical hostname
// (the spec's own choice) and never the value.
func loopHostProblem(ref secretRef) harnessconfig.Problem {
	// An unparsable destination never reaches here: shape validation refuses
	// it earlier, so the empty host is only a defensive fallback.
	var host string
	if u, err := url.Parse(ref.destination); err == nil {
		host = canonicalHost(u.Hostname())
	}
	return harnessconfig.Problem{
		FieldPath: ref.path,
		Message:   fmt.Sprintf("loop secret %q may only be sent to the hosts it lists, and %q is not one of them", ref.name, host),
		RuleID:    harness.RuleSecretRef,
	}
}

package main

// loop_skills_agreement_test.go is change 0134's acceptance-criterion-2 test:
// two servers over ONE installed-skills source advertise identical skills and
// digest before AND after a source change, and both pick up the change after a
// refresh. It exercises the production composition root (loopServerHost +
// startSkillsRefresh + config-driven source selection) against a shared
// FSSource directory.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ethanhinson/fuse/internal/capabilities"
	"github.com/ethanhinson/fuse/internal/config"
	"github.com/ethanhinson/fuse/internal/event"
	"github.com/ethanhinson/fuse/internal/observe"
	"github.com/ethanhinson/fuse/internal/permissions"
	"github.com/ethanhinson/fuse/internal/skills"
)

// agreementOperator is a minimal operator config for the two servers. skills.dirs
// points at the shared source dir assigned per-test (via YAML templating).
const agreementOperator = `
models:
  default: fast
  fast: {id: cloud/fast}
throughput: {requests_per_minute: 6000}
skills:
  dirs: [%s]
  refresh: 40ms
`

// skillsDirWriter writes a skill under dir/<name>/SKILL.md.
func skillsDirWriter(t *testing.T, dir, name, desc, body string) {
	t.Helper()
	d := filepath.Join(dir, name)
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	md := "---\nname: " + name + "\ndescription: " + desc + "\n---\n" + body + "\n"
	if err := os.WriteFile(filepath.Join(d, "SKILL.md"), []byte(md), 0o644); err != nil {
		t.Fatal(err)
	}
}

// twoAgreementHosts builds two production loopServerHosts over ONE shared
// skills source dir, each with its own refresh loop over the source. Returns
// the dir (so the test can mutate it) and the two hosts.
func twoAgreementHosts(t *testing.T) (string, *loopServerHost, *loopServerHost) {
	t.Helper()
	srcDir := t.TempDir()
	skillsDirWriter(t, srcDir, "notes", "take notes", "Take notes.")
	skillsDirWriter(t, srcDir, "review", "review work", "Review work.")

	yaml := strings.ReplaceAll(agreementOperator, "%s", srcDir)
	return srcDir, agreementHost(t, yaml), agreementHost(t, yaml)
}

// agreementHost builds one loopServerHost over the given operator YAML by
// following the binding's own composition path: skillSourceFromConfig for the
// source, startSkillsRefresh for the live atomic set, then the shared deps
// assembly with the live set as both the host skills and the tool lookup.
func agreementHost(t *testing.T, operatorYAML string) *loopServerHost {
	t.Helper()
	cfg, err := config.FromYAML([]byte(operatorYAML), "operator")
	if err != nil {
		t.Fatal(err)
	}
	reg := registryFromConfig(cfg)
	src, err := skillSourceFromConfig(cfg)
	if err != nil {
		t.Fatalf("skillSourceFromConfig: %v", err)
	}
	live, stop, err := startSkillsRefresh(context.Background(), src, cfg.Skills.RefreshDuration(), func() {})
	if err != nil {
		t.Fatalf("startSkillsRefresh: %v", err)
	}
	t.Cleanup(stop)
	skillLookup := func(name string) (skills.Skill, bool) {
		set, _ := live.Load()
		if set == nil {
			return skills.Skill{}, false
		}
		return set.Lookup(name)
	}
	toolReg := defaultToolRegistry(nil, cfg.Research, skillLookup)
	set, _ := live.Load()
	deps := assembleLoopServerRuntimeDeps(nil, cfg, reg, reg.Default, toolReg, rootSystemBlock(cfg, set),
		permissions.AlwaysApprove, sessionRateGate(cfg), observe.NoopObserver{}, nil, nil, live)
	deps.BaseDir = t.TempDir()
	host, ok := deps.Harness.(*loopServerHost)
	if !ok {
		t.Fatal("deps.Harness is not *loopServerHost")
	}
	return host
}

// skillsEqual asserts two advertised skill lists hold the same (name,
// description) pairs regardless of order. Capabilities advertise name and
// description (not the body — the body is what a resume resolves).
func skillsEqual(t *testing.T, a, b []capabilities.Skill) {
	t.Helper()
	if len(a) != len(b) {
		t.Fatalf("skill list lengths differ: %d vs %d", len(a), len(b))
	}
	byName := map[string]capabilities.Skill{}
	for _, s := range b {
		byName[s.Name] = s
	}
	for _, sa := range a {
		sb, ok := byName[sa.Name]
		if !ok {
			t.Errorf("server A has skill %q that B lacks", sa.Name)
			continue
		}
		if sa.Description != sb.Description {
			t.Errorf("skill %q description differs: A=%q B=%q", sa.Name, sa.Description, sb.Description)
		}
	}
}

func hasSkill(list []capabilities.Skill, name string) bool {
	for _, s := range list {
		if s.Name == name {
			return true
		}
	}
	return false
}

// TestTwoServersAgreeOnOneSkillSource is acceptance criterion 2: before and
// after a source change, two servers over one source advertise identical
// Skills and digest, and both report the change after a refresh.
func TestTwoServersAgreeOnOneSkillSource(t *testing.T) {
	srcDir, host1, host2 := twoAgreementHosts(t)
	ctx := context.Background()
	tenant := event.DefaultTenant

	// Before: identical and non-empty.
	c1, err := host1.Capabilities(ctx, tenant)
	if err != nil {
		t.Fatalf("host1 capabilities: %v", err)
	}
	c2, err := host2.Capabilities(ctx, tenant)
	if err != nil {
		t.Fatalf("host2 capabilities: %v", err)
	}
	if c1.SkillsDigest == "" {
		t.Fatal("skills digest must be non-empty")
	}
	if c1.SkillsDigest != c2.SkillsDigest {
		t.Fatalf("digests differ before change: %q vs %q", c1.SkillsDigest, c2.SkillsDigest)
	}
	skillsEqual(t, c1.Skills, c2.Skills)

	// Add a skill to the source.
	skillsDirWriter(t, srcDir, "researchx", "an x skill", "x body")

	// Wait one refresh interval (bounded) for both hosts to pick it up.
	waitFor := time.Now().Add(2 * time.Second)
	for {
		ca, _ := host1.Capabilities(ctx, tenant)
		cb, _ := host2.Capabilities(ctx, tenant)
		if hasSkill(ca.Skills, "researchx") && hasSkill(cb.Skills, "researchx") {
			if ca.SkillsDigest != cb.SkillsDigest {
				t.Fatalf("digests differ after change: %q vs %q", ca.SkillsDigest, cb.SkillsDigest)
			}
			skillsEqual(t, ca.Skills, cb.Skills)
			return
		}
		if time.Now().After(waitFor) {
			t.Fatal("servers did not pick up the new skill within 2s")
		}
		time.Sleep(20 * time.Millisecond)
	}
}
package skills

import "testing"

func testSet(t *testing.T, docs ...string) *Set {
	t.Helper()
	set := &Set{}
	for i, d := range docs {
		sk, err := ParseSkill("mem://"+string(rune('a'+i)), []byte(d))
		if err != nil {
			t.Fatal(err)
		}
		set.skills = append(set.skills, sk)
	}
	return set
}

const ledgerDoc = `---
name: ledger
description: contest problems
activation:
  phrases: ["competitive programming", "contest problem"]
  paths: ["**/solution.py", "samples/**"]
---
# Ledger body
`

const docketDoc = `---
name: docket-status
description: the backlog
activation:
  paths: [".docket/**"]
---
# Docket body
`

const alwaysDoc = `---
name: house-style
description: conventions
activation:
  always: true
---
# Style body
`

func TestParseActivation(t *testing.T) {
	sk := testSet(t, ledgerDoc).All()[0]
	if !sk.Activation.Declared() || len(sk.Activation.Phrases) != 2 || len(sk.Activation.Paths) != 2 {
		t.Fatalf("activation not parsed: %+v", sk.Activation)
	}
	plain := testSet(t, "---\nname: x\ndescription: y\n---\nbody").All()[0]
	if plain.Activation.Declared() {
		t.Fatal("a skill without an activation block must not declare triggers")
	}
}

func TestActivatorNilWhenNothingDeclared(t *testing.T) {
	if NewActivator(testSet(t, "---\nname: x\ndescription: y\n---\nbody")) != nil {
		t.Fatal("expected nil activator")
	}
	var a *Activator
	if a.OnRequest("anything") != nil || a.OnToolCall("bash", `{"command":"ls"}`) != nil {
		t.Fatal("nil activator must be inert")
	}
}

func TestOnRequestPhraseAlwaysAndPath(t *testing.T) {
	a := NewActivator(testSet(t, ledgerDoc, docketDoc, alwaysDoc))
	got := a.OnRequest("You are an elite Competitive Programming coach. Solve the task.")
	names := map[string]string{}
	for _, m := range got {
		names[m.Skill.Name] = m.Reason
	}
	if _, ok := names["ledger"]; !ok {
		t.Fatalf("phrase trigger (case-insensitive) should fire: %v", names)
	}
	if _, ok := names["house-style"]; !ok {
		t.Fatalf("always should fire: %v", names)
	}
	if _, ok := names["docket-status"]; ok {
		t.Fatalf("docket must not fire without a path mention: %v", names)
	}
	// a path mentioned in the request
	b := NewActivator(testSet(t, docketDoc))
	if got := b.OnRequest("Refresh .docket/changes/0071.md and report"); len(got) != 1 || got[0].Skill.Name != "docket-status" {
		t.Fatalf("path mention in request should fire docket: %+v", got)
	}
}

func TestOnToolCallPathsAndOnce(t *testing.T) {
	a := NewActivator(testSet(t, ledgerDoc, docketDoc))
	if got := a.OnToolCall("read_file", `{"path":"README.md"}`); len(got) != 0 {
		t.Fatalf("unrelated path fired: %+v", got)
	}
	got := a.OnToolCall("write_file", `{"path":"work/solution.py","content":"x"}`)
	if len(got) != 1 || got[0].Skill.Name != "ledger" {
		t.Fatalf("**/solution.py should fire ledger: %+v", got)
	}
	if got := a.OnToolCall("bash", `{"command":"python3 solution.py < samples/1.in"}`); len(got) != 0 {
		t.Fatalf("ledger must attach only once: %+v", got)
	}
	got = a.OnToolCall("bash", `{"command":"cat .docket/changes/0071.md"}`)
	if len(got) != 1 || got[0].Skill.Name != "docket-status" {
		t.Fatalf("bash path token under .docket/** should fire docket: %+v", got)
	}
	if got := a.OnRequest("competitive programming"); len(got) != 0 {
		t.Fatalf("already-attached skills must not fire again: %+v", got)
	}
}

func TestGlobMatch(t *testing.T) {
	cases := []struct {
		glob, p string
		want    bool
	}{
		{"**/*.py", "a/b/c.py", true}, {"**/*.py", "c.py", true}, {"**/*.py", "a/b/c.go", false},
		{".docket/**", ".docket/changes/x.md", true}, {".docket/**", "src/.docket/x", true}, {".docket/**", "docket/x", false},
		{"samples/**", "samples/1.in", true}, {"samples/**", "samples", true},
		{"*.md", "README.md", true}, {"*.md", "docs/README.md", true},
	}
	for _, c := range cases {
		if globMatch(c.glob, c.p) != c.want {
			t.Errorf("globMatch(%q, %q) = %v, want %v", c.glob, c.p, !c.want, c.want)
		}
	}
}

func TestMentionedPaths(t *testing.T) {
	got := mentionedPaths(`Read task.md, then edit src/main.go and cat .docket/changes/0071.md (see ./notes/x.txt)`)
	want := map[string]bool{"task.md": true, "src/main.go": true, ".docket/changes/0071.md": true, "./notes/x.txt": true}
	for _, g := range got {
		delete(want, g)
	}
	if len(want) != 0 {
		t.Fatalf("missing %v in %v", want, got)
	}
}

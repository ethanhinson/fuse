package skills

import "testing"

func TestOnlyKeepsTheNamedSkillsInOrder(t *testing.T) {
	s := &Set{skills: []Skill{{Name: "a"}, {Name: "b"}, {Name: "c"}}}
	got := s.Only([]string{"c", "a", "missing", "a"})
	if len(got.All()) != 2 || got.All()[0].Name != "c" || got.All()[1].Name != "a" {
		t.Fatalf("Only = %+v", got.All())
	}
	if _, ok := got.Lookup("b"); ok {
		t.Fatal("a skill outside the subset is still found")
	}
	if len(s.All()) != 3 {
		t.Fatal("Only mutated the source set")
	}
	if n := len((*Set)(nil).Only([]string{"a"}).All()); n != 0 {
		t.Fatalf("nil set subset has %d skills", n)
	}
}

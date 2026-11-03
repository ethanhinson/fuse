package skills

// CarriedPath is the Path of a skill carried in a harness spec: it has no file
// on the server.
const CarriedPath = "spec"

// NewCarried returns a skill a harness spec carries: name, description and
// body exactly as sent, with no frontmatter, slash command or dispatch hints.
// It never goes through the SKILL.md parser, so any description text is kept
// verbatim.
func NewCarried(name, description, body string) Skill {
	return Skill{Name: name, Description: description, Body: body, Path: CarriedPath}
}

// Entry is one skill a loop's spec lists: a reference to an installed skill by
// Name, or, when Carried is set, a skill the spec defines itself.
type Entry struct {
	Name    string
	Carried *Skill
}

// Compose returns a new Set holding the entries' skills in spec order. A
// carried entry contributes its own skill and shadows an installed skill of
// the same name for this set only; a reference contributes the installed skill
// and is skipped when this set does not hold it. The first entry of a name
// wins. A nil set composes from carried entries only. s is never modified.
func (s *Set) Compose(entries []Entry) *Set {
	out := &Set{}
	seen := map[string]bool{}
	for _, e := range entries {
		if seen[e.Name] {
			continue
		}
		if e.Carried != nil {
			seen[e.Name] = true
			out.skills = append(out.skills, *e.Carried)
			continue
		}
		if s == nil {
			continue
		}
		if sk, ok := s.Lookup(e.Name); ok {
			seen[e.Name] = true
			out.skills = append(out.skills, sk)
		}
	}
	return out
}

package permissions

import "testing"

// TestIsReadOnlySafe_Segments drives isReadOnlySafe per segment, exercising the
// flag-inspecting conditionals for find/git/sed and the plain read-only utils.
func TestIsReadOnlySafe_Segments(t *testing.T) {
	cases := []struct {
		desc string
		cmd  string
		want bool
	}{
		// find: mutating/exec actions are unsafe; a plain search is safe.
		{"find -delete is unsafe", "find . -delete", false},
		{"find -exec is unsafe", "find . -name x -exec rm {} ;", false},
		{"find -execdir is unsafe", "find . -execdir foo {} ;", false},
		{"find -fprint is unsafe", "find . -fprint out.txt", false},
		{"find -ok is unsafe", "find . -ok rm {} ;", false},
		{"find -name search is safe", "find . -name x", true},
		{"bare find is safe", "find .", true},

		// git: only read-only subcommands are safe.
		{"git status safe", "git status", true},
		{"git log safe", "git log --oneline", true},
		{"git diff safe", "git diff HEAD~1", true},
		{"git show safe", "git show abc123", true},
		{"git rev-parse safe", "git rev-parse HEAD", true},
		{"git describe safe", "git describe --tags", true},
		{"git blame safe", "git blame file.go", true},
		{"git push unsafe", "git push origin main", false},
		{"git commit unsafe", "git commit -m x", false},
		{"git checkout unsafe (not enumerated)", "git checkout main", false},
		{"bare git unsafe (no read-only subcommand)", "git", false},

		// git branch: list forms safe, mutating forms unsafe.
		{"git branch bare (list) safe", "git branch", true},
		{"git branch --list safe", "git branch --list", true},
		{"git branch -l safe", "git branch -l", true},
		{"git branch -a safe", "git branch -a", true},
		{"git branch -r safe", "git branch -r", true},
		{"git branch -v safe", "git branch -v", true},
		{"git branch newthing unsafe (creates)", "git branch newthing", false},
		{"git branch -d unsafe (deletes)", "git branch -d old", false},
		{"git branch -D unsafe (deletes)", "git branch -D old", false},
		{"git branch -m unsafe (renames)", "git branch -m new", false},

		// git remote: read forms safe, mutating subcommands unsafe.
		{"git remote bare safe", "git remote", true},
		{"git remote -v safe", "git remote -v", true},
		{"git remote show safe", "git remote show origin", true},
		{"git remote add unsafe", "git remote add o url", false},
		{"git remote remove unsafe", "git remote remove o", false},
		{"git remote set-url unsafe", "git remote set-url o url", false},

		// git config: only --get/--list are safe.
		{"git config --get safe", "git config --get user.name", true},
		{"git config --list safe", "git config --list", true},
		{"git config bare set unsafe", "git config user.name bob", false},

		// git tag: only the list forms are safe.
		{"git tag bare (list) safe", "git tag", true},
		{"git tag -l safe", "git tag -l", true},
		{"git tag --list safe", "git tag --list", true},
		{"git tag newtag unsafe (creates)", "git tag newtag", false},

		// sed: only the non-mutating -n …p print form is safe.
		{"sed -n print safe", "sed -n 1,5p file", true},
		{"sed -n print combined flag safe", "sed -np file", true},
		{"sed -i in-place unsafe", "sed -i s/a/b/ file", false},
		{"sed without -n unsafe", "sed s/a/b/ file", false},
		{"sed -ni combined unsafe (in-place)", "sed -ni s/a/b/ file", false},
		{"sed --in-place=.bak unsafe", "sed --in-place=.bak s/a/b/ file", false},

		// plain read-only utilities: safe with any args.
		{"ls safe", "ls -la /", true},
		{"cat safe", "cat file", true},
		{"pwd safe", "pwd", true},
		{"wc safe", "wc -l file", true},
		{"head safe", "head -n5 file", true},
		{"tail safe", "tail -f file", true},
		{"grep safe", "grep -r foo .", true},
		{"rg safe", "rg foo", true},
		{"echo safe", "echo hi", true},
		{"which safe", "which go", true},
		{"dirname safe", "dirname /a/b", true},
		{"basename safe", "basename /a/b", true},
		{"true safe", "true", true},
		{"false safe", "false", true},
		{"test safe", "test -f file", true},

		// unknown command is not on the safe list.
		{"unknown command unsafe", "frobnicate", false},
	}

	for _, tc := range cases {
		t.Run(tc.desc, func(t *testing.T) {
			segs := segsFor(t, tc.cmd)
			if len(segs) != 1 {
				t.Fatalf("splitSegments(%q) = %d segments, want 1", tc.cmd, len(segs))
			}
			if got := isReadOnlySafe(segs[0]); got != tc.want {
				t.Errorf("isReadOnlySafe(%q) = %v, want %v", tc.cmd, got, tc.want)
			}
		})
	}
}

// TestAllSegmentsReadOnlySafe_ANDing proves the per-segment AND: every segment
// must be independently safe for the whole command to be safe.
func TestAllSegmentsReadOnlySafe_ANDing(t *testing.T) {
	cases := []struct {
		desc string
		cmd  string
		want bool
	}{
		{"two safe segments => safe", "git status && git log", true},
		{"safe then unsafe git => unsafe", "git status && git push", false},
		{"safe then in-place sed => unsafe", "cat f && sed -i s/a/b/ file", false},
		{"three safe segments => safe", "ls; pwd; git diff", true},
		{"empty segment set => not safe", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.desc, func(t *testing.T) {
			segs := segsFor(t, tc.cmd)
			if got := allSegmentsReadOnlySafe(segs); got != tc.want {
				t.Errorf("allSegmentsReadOnlySafe(%q) = %v, want %v", tc.cmd, got, tc.want)
			}
		})
	}
}

// TestReadOnlySafe_PathQualifiedNeverReaches confirms a path-qualified argv[0]
// (./sed) never reaches the safe list: the parser fails it closed before
// isReadOnlySafe is ever consulted.
func TestReadOnlySafe_PathQualifiedNeverReaches(t *testing.T) {
	if _, err := splitSegments("./sed -n 1,5p file"); err == nil {
		t.Fatalf("splitSegments(./sed …) parsed; want ErrUnparseable so ./sed never classifies safe")
	}
	// And basename-only sed (the parser only ever hands isReadOnlySafe a
	// basename) classifies on the sed rules, not a raw path.
	seg := Segment{Name: "sed", Args: []string{"-n", "1,5p", "file"}}
	if !isReadOnlySafe(seg) {
		t.Fatalf("isReadOnlySafe(sed -n …p) = false, want true")
	}
}

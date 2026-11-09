package tui

import (
	"reflect"
	"testing"
)

func TestParseDroppedPaths(t *testing.T) {
	cases := []struct {
		name, in, goos string
		want           []string
		ok             bool
	}{
		{"plain", `/a/b.png`, "darwin", []string{"/a/b.png"}, true},
		{"escaped space trailing", "/a/my\\ shot.png ", "darwin", []string{"/a/my shot.png"}, true},
		{"single quotes", `'/a/my shot.png'`, "darwin", []string{"/a/my shot.png"}, true},
		{"double quotes", `"/a/my shot.png"`, "darwin", []string{"/a/my shot.png"}, true},
		{"two spaces", `/a/x.png /b/y.png`, "darwin", []string{"/a/x.png", "/b/y.png"}, true},
		{"two newline", "/a/x.png\n/b/y.png", "darwin", []string{"/a/x.png", "/b/y.png"}, true},
		{"escaped quote", `/a/it\'s.png`, "darwin", []string{"/a/it's.png"}, true},
		{"mid quote", `/a/'my shot'.png`, "darwin", []string{"/a/my shot.png"}, true},
		{"narrow nbsp", "/a/Screenshot\\ 10.12\u202fAM.png", "darwin", []string{"/a/Screenshot 10.12\u202fAM.png"}, true},
		{"escaped dq", `"/a/q\"x.png"`, "darwin", []string{`/a/q"x.png`}, true},
		{"unterminated", `'/a/b.png`, "darwin", nil, false},
		{"trailing backslash", `/a/b\`, "darwin", nil, false},
		{"windows", `"C:\a\b c.png"`, "windows", []string{`C:\a\b c.png`}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := parseDroppedPaths(c.in, c.goos)
			if ok != c.ok || !reflect.DeepEqual(got, c.want) {
				t.Fatalf("got %q ok=%v, want %q ok=%v", got, ok, c.want, c.ok)
			}
		})
	}
}

func TestResolveDroppedPath(t *testing.T) {
	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		{"file:///a/my%20shot.png", "/a/my shot.png", true},
		{"file://localhost/a/b.png", "/a/b.png", true},
		{"file://host/a/b.png", "", false},
		{"~/p.png", "/home/u/p.png", true},
		{"~bob/p.png", "", false},
		{"rel/p.png", "", false},
		{"/a/../b.png", "/b.png", true},
	}
	for _, c := range cases {
		got, ok := resolveDroppedPath(c.in, "/home/u", "darwin")
		if got != c.want || ok != c.ok {
			t.Errorf("%q: got %q %v, want %q %v", c.in, got, ok, c.want, c.ok)
		}
	}
}

func TestParseAttachArgsAllowsRelative(t *testing.T) {
	got, ok := parseAttachArgs("shot.png", "/h", "/w", "darwin")
	if !ok || !reflect.DeepEqual(got, []string{"/w/shot.png"}) {
		t.Fatalf("got %q %v", got, ok)
	}
	if _, ok := parseAttachArgs("~bob/x.png", "/h", "/w", "darwin"); ok {
		t.Error("~user should be rejected")
	}
	if _, ok := parseDrop("shot.png", "/h", "darwin"); ok {
		t.Error("relative drop should be rejected")
	}
}

package main

import "testing"

func TestParseRouteDecision(t *testing.T) {
	cases := []struct{ in, mode, handle string }{
		{`{"mode":"direct","handle":"@coder"}`, "direct", "@coder"},
		{"blah blah {\"mode\":\"direct\",\"handle\":\"@x\"} trailing", "direct", "@x"},
		{`{"mode":"queued","handle":""}`, "queued", ""},
		{"not json at all", "queued", ""},
		{`{"mode":"weird"}`, "queued", ""},
	}
	for _, c := range cases {
		d := parseRouteDecision(c.in)
		if d.ModeStr != c.mode || d.Handle != c.handle {
			t.Errorf("parse(%q) = {%q,%q}, want {%q,%q}", c.in, d.ModeStr, d.Handle, c.mode, c.handle)
		}
	}
}

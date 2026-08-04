package agent

import "testing"

func TestFingerprintStable(t *testing.T) {
	a := fingerprint("bash", `{"command":"ls"}`)
	b := fingerprint("bash", `{"command":"ls"}`)
	if a != b {
		t.Fatal("same call must fingerprint equally")
	}
	if a == fingerprint("bash", `{"command":"pwd"}`) {
		t.Fatal("different args must differ")
	}
}

func TestLoopDetectorAbortsAfterThreeRepeats(t *testing.T) {
	d := newLoopDetector(3)
	fps := []string{fingerprint("bash", `{"command":"ls"}`)}
	if d.seen(fps) {
		t.Fatal("first occurrence must not trip")
	}
	if d.seen(fps) {
		t.Fatal("second occurrence must not trip")
	}
	if !d.seen(fps) {
		t.Fatal("third identical occurrence must trip")
	}
}

func TestLoopDetectorResetsOnChange(t *testing.T) {
	d := newLoopDetector(3)
	ls := []string{fingerprint("bash", `{"command":"ls"}`)}
	pwd := []string{fingerprint("bash", `{"command":"pwd"}`)}
	d.seen(ls)
	d.seen(ls)
	if d.seen(pwd) {
		t.Fatal("a different call must reset the counter")
	}
	if d.seen(pwd) {
		t.Fatal("second pwd must not trip")
	}
}

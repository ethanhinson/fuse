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

// TestLoopDetectorCatchesPeriod2 is the regression test for #24: a strict ABAB
// alternation of two distinct tool-call sets must trip, which the old
// last+count detector could never see (each switch reset the counter).
func TestLoopDetectorCatchesPeriod2(t *testing.T) {
	d := newLoopDetector(3)
	a := []string{fingerprint("bash", `{"command":"ls"}`)}
	b := []string{fingerprint("bash", `{"command":"pwd"}`)}
	// A,B,A,B,A -> not yet a full 2*limit window.
	seq := []([]string){a, b, a, b, a}
	for i, s := range seq {
		if d.seen(s) {
			t.Fatalf("tripped early at index %d (need 2*limit=6 keys)", i)
		}
	}
	// The 6th key completes A,B,A,B,A,B -> period-2 over 3 repeats -> trip.
	if !d.seen(b) {
		t.Fatal("full ABABAB alternation must trip the period-2 detector")
	}
}

// TestLoopDetectorNoFalsePositiveOnVariedWork guards against tripping on
// legitimate work that merely alternates tool NAMES but hits different targets,
// so each turn's fingerprint set is distinct (never collapses to two keys).
func TestLoopDetectorNoFalsePositiveOnVariedWork(t *testing.T) {
	d := newLoopDetector(3)
	// read A, write A, read B, write B, read C, write C — six DISTINCT sets.
	seq := []([]string){
		{fingerprint("read_file", `{"path":"a"}`)},
		{fingerprint("write_file", `{"path":"a"}`)},
		{fingerprint("read_file", `{"path":"b"}`)},
		{fingerprint("write_file", `{"path":"b"}`)},
		{fingerprint("read_file", `{"path":"c"}`)},
		{fingerprint("write_file", `{"path":"c"}`)},
	}
	for i, s := range seq {
		if d.seen(s) {
			t.Fatalf("varied work must not trip (index %d)", i)
		}
	}
}

// TestLoopDetectorResetClearsWindow confirms reset() forces another full window
// before the detector can trip again (the force-through-after-approval path).
func TestLoopDetectorResetClearsWindow(t *testing.T) {
	d := newLoopDetector(3)
	a := []string{fingerprint("bash", `{"command":"ls"}`)}
	d.seen(a)
	d.seen(a)
	if !d.seen(a) {
		t.Fatal("third identical must trip")
	}
	d.reset()
	if d.seen(a) {
		t.Fatal("after reset, one occurrence must not immediately re-trip")
	}
	if d.seen(a) {
		t.Fatal("after reset, two occurrences must not trip")
	}
	if !d.seen(a) {
		t.Fatal("after reset, third occurrence must trip again")
	}
}

package banner

import (
	"bytes"
	"strings"
	"testing"
)

func TestString_ContainsVersion(t *testing.T) {
	got := String("9.9.9")
	for _, want := range []string{
		"9.9.9",
		`\____/`,
		"fuse run",
		"fuse mcps",
		"fuse help",
		"multi-model agent harness",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("String() missing %q\noutput:\n%s", want, got)
		}
	}
}

func TestPrint_WritesSameAsString(t *testing.T) {
	var buf bytes.Buffer
	Print(&buf, "1.2.3")
	if buf.Len() == 0 {
		t.Fatal("Print wrote empty output")
	}
	if got, want := buf.String(), String("1.2.3"); got != want {
		t.Errorf("Print output = %q, want %q", got, want)
	}
}

func TestString_NoTabsNoEsc(t *testing.T) {
	got := String("0.0.0")
	if strings.ContainsRune(got, '\t') {
		t.Error("String output contains a tab")
	}
	if strings.ContainsRune(got, '\x1b') {
		t.Error("String output contains an ESC byte")
	}
}

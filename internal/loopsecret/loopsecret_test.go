package loopsecret

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os/exec"
	"reflect"
	"strings"
	"testing"
)

const sentinel = "v-SENTINEL-123"

func TestSecretNeverRendersItsValue(t *testing.T) {
	s := Secret{Name: "my-token", Value: sentinel, Hosts: []string{"mcp.acme.test"}}

	jsonOut, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	var logBuf bytes.Buffer
	slog.New(slog.NewJSONHandler(&logBuf, nil)).Info("x", "s", s)

	renderings := map[string]string{
		"Sprint":           fmt.Sprint(s),
		"%v":               fmt.Sprintf("%v", s),
		"%+v":              fmt.Sprintf("%+v", s),
		"%#v":              fmt.Sprintf("%#v", s),
		"%s":               fmt.Sprintf("%s", s),
		"%q":               fmt.Sprintf("%q", s),
		"%+v slice":        fmt.Sprintf("%+v", []Secret{s}),
		"%+v nested slice": fmt.Sprintf("%+v", struct{ S []Secret }{[]Secret{s}}),
		"String":           s.String(),
		"GoString":         s.GoString(),
		"json":             string(jsonOut),
		"slog":             logBuf.String(),
	}
	for name, out := range renderings {
		if strings.Contains(out, sentinel) {
			t.Errorf("%s rendering leaks the value: %s", name, out)
		}
		if !strings.Contains(out, "my-token") {
			t.Errorf("%s rendering lacks the name: %s", name, out)
		}
		if !strings.Contains(out, "[REDACTED]") {
			t.Errorf("%s rendering lacks the redaction marker: %s", name, out)
		}
	}
}

func TestCheckLimits(t *testing.T) {
	if err := CheckLimits(nil); err != nil {
		t.Fatalf("nil slice: %v", err)
	}
	if err := CheckLimits([]Secret{}); err != nil {
		t.Fatalf("empty slice: %v", err)
	}

	full := make([]Secret, MaxSecrets)
	for i := range full {
		full[i] = Secret{Name: fmt.Sprintf("s%d", i), Value: strings.Repeat("a", MaxValueBytes), Hosts: []string{"h.test"}}
	}
	if err := CheckLimits(full); err != nil {
		t.Fatalf("32 entries of 16384 bytes should pass: %v", err)
	}

	tooMany := append(append([]Secret{}, full...), Secret{Name: "extra", Value: "x", Hosts: []string{"h.test"}})
	err := CheckLimits(tooMany)
	if err == nil {
		t.Fatal("33 entries should fail")
	}
	if !strings.Contains(err.Error(), "33") || !strings.Contains(err.Error(), "32") {
		t.Fatalf("count error should name 33 and 32: %v", err)
	}

	big := sentinel + strings.Repeat("b", MaxValueBytes+1-len(sentinel))
	err = CheckLimits([]Secret{{Name: "big", Value: big, Hosts: []string{"h.test"}}})
	if err == nil {
		t.Fatal("16385-byte value should fail")
	}
	msg := err.Error()
	if !strings.Contains(msg, "secrets[0]") || !strings.Contains(msg, "16385") {
		t.Fatalf("size error should name secrets[0] and 16385: %v", err)
	}
	if strings.Contains(msg, sentinel) {
		t.Fatalf("size error leaks the value: %v", err)
	}
}

func TestContextRoundTrip(t *testing.T) {
	if got := From(context.Background()); got != nil {
		t.Fatalf("From(empty ctx) = %v, want nil", got)
	}

	ctx := context.Background()
	if got := WithSecrets(ctx, nil); got != ctx {
		t.Fatal("WithSecrets(ctx, nil) should return ctx unchanged")
	}

	ss := []Secret{{Name: "a", Value: "va", Hosts: []string{"a.test", "b.test"}}}
	want := []Secret{{Name: "a", Value: "va", Hosts: []string{"a.test", "b.test"}}}
	got := WithSecrets(ctx, ss)
	if !reflect.DeepEqual(From(got), want) {
		t.Fatalf("From = %#v, want %#v", From(got), want)
	}

	ss[0].Name = "mutated"
	ss[0].Hosts[0] = "evil.test"
	if !reflect.DeepEqual(From(got), want) {
		t.Fatal("WithSecrets must store a defensive copy of the slice and its Hosts")
	}
}

func TestIsAStdlibOnlyLeaf(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go not on PATH; skipping dependency-graph guard")
	}
	const self = "github.com/ethanhinson/fuse/internal/loopsecret"
	out, err := exec.Command("go", "list", "-deps", self).CombinedOutput()
	if err != nil {
		t.Fatalf("go list -deps: %v\n%s", err, out)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "github.com/ethanhinson/fuse/") && line != self {
			t.Errorf("internal/loopsecret must import only the standard library, but depends on %s", line)
		}
	}
}

package harnessv1_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

// TestGeneratedStubsHaveNoDrift is the generate-and-commit drift guard for
// fuse.harness.v1 (change 0084), the sibling of internal/loopwire/v1's guard: it
// re-runs `buf generate` into a throwaway tree with the same plugins and options
// as proto/buf.gen.yaml and asserts the committed Go + TS stubs are
// byte-identical. It SKIPS when the codegen toolchain is not on PATH.
func TestGeneratedStubsHaveNoDrift(t *testing.T) {
	for _, tool := range []string{"buf", "protoc-gen-go", "protoc-gen-connect-go"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not on PATH; drift guard needs the codegen toolchain (skipping)", tool)
		}
	}

	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate test file path")
	}
	// thisFile = <repo>/harness/v1/drift_test.go
	repoRoot := filepath.Clean(filepath.Join(filepath.Dir(thisFile), "..", ".."))
	protoDir := filepath.Join(repoRoot, "proto")
	tmp := t.TempDir()

	template := "" +
		"version: v2\n" +
		"plugins:\n" +
		"  - local: protoc-gen-go\n" +
		"    out: " + tmp + "/go\n" +
		"    opt:\n" +
		"      - module=github.com/ethanhinson/fuse\n" +
		"  - local: protoc-gen-connect-go\n" +
		"    out: " + tmp + "/go\n" +
		"    opt:\n" +
		"      - module=github.com/ethanhinson/fuse\n" +
		"  - local: [\"npx\", \"protoc-gen-es\"]\n" +
		"    out: " + tmp + "/ts\n" +
		"    opt:\n" +
		"      - target=ts\n" +
		"      - import_extension=js\n" +
		"      - rewrite_imports=./buf/validate/**/*_pb.js:@bufbuild/protovalidate/gen\n"
	tmplPath := filepath.Join(tmp, "buf.gen.drift.yaml")
	if err := os.WriteFile(tmplPath, []byte(template), 0o644); err != nil {
		t.Fatalf("write drift template: %v", err)
	}
	cmd := exec.Command("buf", "generate", "--template", tmplPath)
	cmd.Dir = protoDir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("buf generate into tmp failed (%v; TS plugin or buf deps may be unavailable); output:\n%s", err, out)
	}

	for _, rel := range []struct{ committed, regen string }{
		{"harness/v1/harness.pb.go", "go/harness/v1/harness.pb.go"},
		{"harness/v1/options.pb.go", "go/harness/v1/options.pb.go"},
		{"harness/v1/harnessv1connect/harness.connect.go", "go/harness/v1/harnessv1connect/harness.connect.go"},
		{"proto/gen/ts/fuse/harness/v1/harness_pb.ts", "ts/fuse/harness/v1/harness_pb.ts"},
		{"proto/gen/ts/fuse/harness/v1/options_pb.ts", "ts/fuse/harness/v1/options_pb.ts"},
	} {
		regen, err := os.ReadFile(filepath.Join(tmp, rel.regen))
		if err != nil {
			t.Fatalf("read regenerated %s: %v", rel.regen, err)
		}
		committed, err := os.ReadFile(filepath.Join(repoRoot, rel.committed))
		if err != nil {
			t.Fatalf("read committed %s: %v", rel.committed, err)
		}
		if string(regen) != string(committed) {
			t.Errorf("DRIFT: %s differs from `buf generate` output - run `make proto` and commit", rel.committed)
		}
	}
}

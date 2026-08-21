package sandbox_test

import (
	"os"
	"reflect"
	"testing"

	"github.com/ethanhinson/fuse/internal/tools/sandbox"
)

// fakeLookup builds a lookup over a fixed host environment so tests never
// mutate the real process environment.
func fakeLookup(host map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) {
		v, ok := host[k]
		return v, ok
	}
}

func TestResolveEnv(t *testing.T) {
	// A secret that is present in every simulated host environment. It must
	// never appear in a resolved Env: there is no inheritance path.
	const secretKey = "FUSE_TEST_SECRET"

	cases := []struct {
		name        string
		host        map[string]string
		passthrough []string
		want        map[string]string
	}{
		{
			name: "base allowlist only, all present",
			host: map[string]string{
				"PATH": "/usr/bin", "HOME": "/home/u", "LANG": "C.UTF-8",
				secretKey: "hunter2", "AWS_SECRET_ACCESS_KEY": "nope",
			},
			want: map[string]string{"PATH": "/usr/bin", "HOME": "/home/u", "LANG": "C.UTF-8"},
		},
		{
			name: "absent base keys are silently omitted, never empty entries",
			host: map[string]string{"PATH": "/bin", secretKey: "hunter2"},
			want: map[string]string{"PATH": "/bin"},
		},
		{
			name:        "operator passthrough included when set",
			host:        map[string]string{"PATH": "/bin", "GOFLAGS": "-mod=mod", secretKey: "hunter2"},
			passthrough: []string{"GOFLAGS"},
			want:        map[string]string{"PATH": "/bin", "GOFLAGS": "-mod=mod"},
		},
		{
			name:        "operator passthrough omitted when unset",
			host:        map[string]string{"PATH": "/bin", secretKey: "hunter2"},
			passthrough: []string{"GOFLAGS"},
			want:        map[string]string{"PATH": "/bin"},
		},
		{
			name:        "passthrough cannot smuggle a var that is not on the host",
			host:        map[string]string{secretKey: "hunter2"},
			passthrough: []string{"NOT_SET_ANYWHERE"},
			want:        map[string]string{},
		},
		{
			name:        "passthrough may explicitly re-request a base key",
			host:        map[string]string{"PATH": "/bin", "HOME": "/h", secretKey: "hunter2"},
			passthrough: []string{"HOME"},
			want:        map[string]string{"PATH": "/bin", "HOME": "/h"},
		},
		{
			name:        "empty-valued host var is passed through as empty, not dropped",
			host:        map[string]string{"PATH": "", secretKey: "hunter2"},
			passthrough: nil,
			want:        map[string]string{"PATH": ""},
		},
		{
			name: "nothing resolves: Allow is non-nil and empty",
			host: map[string]string{secretKey: "hunter2"},
			want: map[string]string{},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := sandbox.ResolveEnv(tc.passthrough, fakeLookup(tc.host))

			if got.Allow == nil {
				t.Fatal("Env.Allow is nil; callers must never see nil (nil means inherit)")
			}
			if _, leaked := got.Allow[secretKey]; leaked {
				t.Fatalf("%s leaked into Env.Allow: %v", secretKey, got.Allow)
			}
			if !reflect.DeepEqual(got.Allow, tc.want) {
				t.Fatalf("Env.Allow = %v, want %v", got.Allow, tc.want)
			}
		})
	}
}

func TestResolveEnvPassthroughCannotDisableScrub(t *testing.T) {
	host := map[string]string{"PATH": "/bin", "FUSE_TEST_SECRET": "hunter2"}
	got := sandbox.ResolveEnv([]string{"", "  "}, fakeLookup(host))
	if _, leaked := got.Allow["FUSE_TEST_SECRET"]; leaked {
		t.Fatalf("secret leaked: %v", got.Allow)
	}
	if len(got.Allow) != 1 {
		t.Fatalf("Env.Allow = %v, want only PATH", got.Allow)
	}
}

func TestResolveEnvFromOSDropsAmbientSecret(t *testing.T) {
	t.Setenv("FUSE_TEST_SECRET", "hunter2")
	if _, ok := os.LookupEnv("FUSE_TEST_SECRET"); !ok {
		t.Fatal("precondition: FUSE_TEST_SECRET must be set in the test process")
	}

	got := sandbox.ResolveEnvFromOS(nil)
	if got.Allow == nil {
		t.Fatal("Env.Allow is nil; callers must never see nil (nil means inherit)")
	}
	if _, leaked := got.Allow["FUSE_TEST_SECRET"]; leaked {
		t.Fatalf("FUSE_TEST_SECRET leaked into Env.Allow: %v", got.Allow)
	}
	for k := range got.Allow {
		switch k {
		case "PATH", "HOME", "LANG":
		default:
			t.Fatalf("unexpected key %q in Env.Allow: %v", k, got.Allow)
		}
	}

	t.Setenv("FUSE_TEST_PASSTHROUGH", "ok")
	withPass := sandbox.ResolveEnvFromOS([]string{"FUSE_TEST_PASSTHROUGH"})
	if withPass.Allow["FUSE_TEST_PASSTHROUGH"] != "ok" {
		t.Fatalf("operator passthrough not honoured: %v", withPass.Allow)
	}
	if _, leaked := withPass.Allow["FUSE_TEST_SECRET"]; leaked {
		t.Fatalf("FUSE_TEST_SECRET leaked into Env.Allow: %v", withPass.Allow)
	}
}

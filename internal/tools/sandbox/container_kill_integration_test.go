package sandbox

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestContainerStoppedOnCancelAgainstRealCLI proves what the fake CLI cannot: that
// killing the `run` client really leaves a container running, and that the kill by
// name really stops it and its writes to the mount. Gated like the smoke test, and
// it only ever touches the container it started, by that container's name.
func TestContainerStoppedOnCancelAgainstRealCLI(t *testing.T) {
	var cli string
	for _, c := range containerCLIs {
		if _, err := exec.LookPath(c); err == nil {
			cli = c
			break
		}
	}
	if cli == "" {
		t.Skipf("skipping: none of %s found on PATH", strings.Join(containerCLIs[:], ", "))
	}
	setup, cancelSetup := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancelSetup()
	if out, err := exec.CommandContext(setup, cli, "pull", DefaultContainerImage).CombinedOutput(); err != nil {
		t.Skipf("skipping: could not pull %s: %v\n%s", DefaultContainerImage, err, out)
	}

	cases := map[string]func() (context.Context, context.CancelFunc){
		"timeout": func() (context.Context, context.CancelFunc) {
			return context.WithTimeout(context.Background(), 3*time.Second)
		},
		"cancel": func() (context.Context, context.CancelFunc) {
			ctx, cancel := context.WithCancel(context.Background())
			time.AfterFunc(3*time.Second, cancel)
			return ctx, cancel
		},
	}
	for name, mkCtx := range cases {
		t.Run(name, func(t *testing.T) {
			root := trustedTestRoot(t)
			var mu sync.Mutex
			var container string
			var killCode int
			killed := false

			h, err := newContainerHandler(Config{Image: DefaultContainerImage},
				withTrustedRoot(root),
				withExecRunner(func(ctx context.Context, bin string, args ...string) ([]byte, int, error) {
					if args[0] == "run" {
						n, _ := argValue(args, "--name")
						mu.Lock()
						container = n
						mu.Unlock()
					}
					out, code, err := runCommand(ctx, bin, nil, args...)
					if args[0] == "kill" {
						mu.Lock()
						killed, killCode = true, code
						mu.Unlock()
					}
					return out, code, err
				}),
			)
			if err != nil {
				t.Skipf("skipping: newContainerHandler: %v", err)
			}
			r, err := h.Acquire(context.Background(), Principal{}, ResolveEnvFromOS(nil))
			if err != nil {
				t.Fatalf("Acquire: %v", err)
			}
			t.Cleanup(func() {
				if container != "" {
					_ = exec.Command(cli, "rm", "-f", container).Run()
				}
			})

			ctx, cancel := mkCtx()
			defer cancel()
			out, err := r.Exec(ctx, "while true; do date >> /workspace/tick; sleep 0.2; done", "")
			if err != nil {
				t.Fatalf("Exec: %v\n%s", err, out.Combined)
			}
			if !killed || killCode != 0 {
				t.Fatalf("killed=%v code=%d output=%q", killed, killCode, out.Combined)
			}
			if strings.Contains(string(out.Combined), "may still be running") {
				t.Fatalf("kill reported as unconfirmed: %q", out.Combined)
			}

			tick := filepath.Join(root, "tick")
			before, err := os.ReadFile(tick)
			if err != nil || len(before) == 0 {
				t.Fatalf("the command never wrote to the mount: %v", err)
			}
			// Well past the loop's 0.2s period.
			time.Sleep(1500 * time.Millisecond)
			after, err := os.ReadFile(tick)
			if err != nil {
				t.Fatal(err)
			}
			if len(after) != len(before) {
				t.Errorf("still writing to the mount after the kill: %d -> %d bytes", len(before), len(after))
			}
			ps, err := exec.Command(cli, "ps", "-q", "--filter", "name=^"+container+"$").CombinedOutput()
			if err != nil {
				t.Fatalf("ps: %v\n%s", err, ps)
			}
			if strings.TrimSpace(string(ps)) != "" {
				t.Errorf("container %s still running", container)
			}
			all, err := exec.Command(cli, "ps", "-aq", "--filter", "name=^"+container+"$", "--filter", "label=fuse.managed").CombinedOutput()
			if err != nil {
				t.Fatalf("ps -a: %v\n%s", err, all)
			}
			if strings.TrimSpace(string(all)) != "" {
				t.Errorf("container %s not removed by --rm", container)
			}
		})
	}
}

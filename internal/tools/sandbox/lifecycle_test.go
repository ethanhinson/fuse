package sandbox

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// stripContainerIdentity checks an observed `run` argv carries its per-run identity
// flags, and returns the argv without them so a whole-argv golden stays exact.
func stripContainerIdentity(t *testing.T, args []string) []string {
	t.Helper()
	name, ok := argValue(args, "--name")
	if !ok || !strings.HasPrefix(name, "fuse-") {
		t.Fatalf("argv has no fuse --name: %v", args)
	}
	if label, _ := argValue(args, "--label"); label != "fuse.managed=true" {
		t.Fatalf("argv has no fuse.managed label: %v", args)
	}
	out := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		if args[i] == "--name" || args[i] == "--label" {
			i++
			continue
		}
		out = append(out, args[i])
	}
	return out
}

// fakeCLI stands in for the docker binary. `run` behaves as the test sets; `kill`
// is recorded along with whether the context it arrived on could still do anything.
type fakeCLI struct {
	mu      sync.Mutex
	runs    [][]string
	kills   [][]string
	killCtx []context.Context
	onRun   func(ctx context.Context) ([]byte, int, error)
	onKill  func(ctx context.Context) ([]byte, int, error)
}

func (f *fakeCLI) run(ctx context.Context, _ string, args ...string) ([]byte, int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch args[0] {
	case "pull":
		return nil, 0, nil
	case "kill":
		f.kills = append(f.kills, append([]string(nil), args...))
		f.killCtx = append(f.killCtx, ctx)
		if f.onKill != nil {
			return f.onKill(ctx)
		}
		return nil, 0, nil
	}
	f.runs = append(f.runs, append([]string(nil), args...))
	onRun := f.onRun
	if onRun == nil {
		return []byte("ok"), 0, nil
	}
	f.mu.Unlock()
	defer f.mu.Lock()
	return onRun(ctx)
}

func untilCancelled(ctx context.Context) ([]byte, int, error) {
	<-ctx.Done()
	return []byte("partial"), -1, nil
}

func fakeCLIRunner(t *testing.T, cli *fakeCLI) Runner {
	t.Helper()
	h, err := newContainerHandler(DefaultConfig(),
		withLookPath(fakeLookPath("docker")),
		withExecRunner(cli.run),
		withTrustedRoot(trustedTestRoot(t)),
	)
	if err != nil {
		t.Fatal(err)
	}
	r, err := h.Acquire(context.Background(), Principal{}, Env{Allow: map[string]string{"PATH": "/bin"}})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestContainerExec_NamedAndLabelled_Unique(t *testing.T) {
	cli := &fakeCLI{}
	r := fakeCLIRunner(t, cli)
	seen := map[string]bool{}
	for range 20 {
		if _, err := r.Exec(context.Background(), "true", ""); err != nil {
			t.Fatal(err)
		}
	}
	for _, args := range cli.runs {
		stripContainerIdentity(t, args) // asserts both flags are present
		name, _ := argValue(args, "--name")
		if seen[name] {
			t.Fatalf("name %s reused", name)
		}
		seen[name] = true
	}
}

// Identity goes before the image: anything after it is the command's own argv.
func TestContainerExec_IdentityFlagsPrecedeImage(t *testing.T) {
	cli := &fakeCLI{}
	r := fakeCLIRunner(t, cli)
	if _, err := r.Exec(context.Background(), "true", ""); err != nil {
		t.Fatal(err)
	}
	args := cli.runs[0]
	image := -1
	for i, a := range args {
		if a == DefaultContainerImage {
			image = i
		}
	}
	for i, a := range args {
		if (a == "--name" || a == "--label") && i > image {
			t.Fatalf("%s after the image: %v", a, args)
		}
	}
}

func TestContainerExec_NormalExitSendsNoKill(t *testing.T) {
	for name, code := range map[string]int{"success": 0, "failure": 3} {
		t.Run(name, func(t *testing.T) {
			cli := &fakeCLI{onRun: func(context.Context) ([]byte, int, error) { return nil, code, nil }}
			r := fakeCLIRunner(t, cli)
			out, err := r.Exec(context.Background(), "true", "")
			if err != nil || out.ExitCode != code {
				t.Fatalf("out %+v err %v", out, err)
			}
			if len(cli.kills) != 0 {
				t.Fatalf("killed a container that exited by itself: %v", cli.kills)
			}
		})
	}
}

func TestContainerExec_TimeoutKillsContainerByName(t *testing.T) {
	cli := &fakeCLI{onRun: untilCancelled}
	r := fakeCLIRunner(t, cli)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	out, err := r.Exec(ctx, "sleep 100", "")
	if err != nil {
		t.Fatal(err)
	}
	if !out.TimedOut || out.ExitCode == 0 {
		t.Fatalf("out %+v", out)
	}
	assertKilledRunContainer(t, cli)
	if !bytes.Contains(out.Combined, []byte("partial")) {
		t.Errorf("output lost: %q", out.Combined)
	}
}

func TestContainerExec_CancelKillsContainerByName(t *testing.T) {
	cli := &fakeCLI{onRun: untilCancelled}
	r := fakeCLIRunner(t, cli)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(10 * time.Millisecond)
		cancel()
	}()
	out, err := r.Exec(ctx, "sleep 100", "")
	if err != nil {
		t.Fatal(err)
	}
	if out.TimedOut {
		t.Errorf("a cancel reported as a timeout")
	}
	assertKilledRunContainer(t, cli)
}

// The kill has to outlive the context that triggered it, and be bounded by its own.
func assertKilledRunContainer(t *testing.T, cli *fakeCLI) {
	t.Helper()
	if len(cli.runs) != 1 || len(cli.kills) != 1 {
		t.Fatalf("runs %v kills %v", cli.runs, cli.kills)
	}
	name, _ := argValue(cli.runs[0], "--name")
	if want := []string{"kill", name}; fmt.Sprint(cli.kills[0]) != fmt.Sprint(want) {
		t.Fatalf("kill argv = %v, want %v", cli.kills[0], want)
	}
	deadline, ok := cli.killCtx[0].Deadline()
	if !ok {
		t.Fatal("kill has no timeout")
	}
	if left := time.Until(deadline); left <= 0 || left > containerKillTimeout {
		t.Fatalf("kill deadline in %s, want within (0, %s]", left, containerKillTimeout)
	}
}

// A daemon that hangs on the kill costs the bound and no more, and the model is told.
func TestContainerExec_HungKillIsBoundedAndReported(t *testing.T) {
	var waited time.Duration
	cli := &fakeCLI{
		onRun: untilCancelled,
		onKill: func(ctx context.Context) ([]byte, int, error) {
			deadline, _ := ctx.Deadline()
			waited = time.Until(deadline)
			return nil, -1, errors.New("signal: killed")
		},
	}
	r := fakeCLIRunner(t, cli)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	out, err := r.Exec(ctx, "sleep 100", "")
	if err != nil {
		t.Fatal(err)
	}
	if waited <= 0 || waited > containerKillTimeout {
		t.Errorf("kill budget %s", waited)
	}
	if !strings.Contains(string(out.Combined), "may still be running") {
		t.Errorf("failed kill not reported: %q", out.Combined)
	}
}

func TestContainerExec_KillFailureReporting(t *testing.T) {
	cases := []struct {
		name, stderr string
		warned       bool
	}{
		{"docker_gone", "Error response from daemon: No such container: fuse-x", false},
		{"docker_stopped", "Error response from daemon: Cannot kill container: fuse-x: container is not running", false},
		{"podman_gone", "Error: no container with name or ID \"fuse-x\" found: no such container", false},
		{"daemon_down", "Cannot connect to the Docker daemon at unix:///var/run/docker.sock.", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cli := &fakeCLI{
				onRun:  untilCancelled,
				onKill: func(context.Context) ([]byte, int, error) { return []byte(c.stderr), 1, nil },
			}
			r := fakeCLIRunner(t, cli)
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			out, err := r.Exec(ctx, "sleep 100", "")
			if err != nil {
				t.Fatal(err)
			}
			if got := strings.Contains(string(out.Combined), "may still be running"); got != c.warned {
				t.Errorf("warned = %v, want %v: %q", got, c.warned, out.Combined)
			}
		})
	}
}

// The real client path: output is bounded, and the client's own exit is still a
// result. /bin/sh stands in for the CLI.
func TestRunCommand_OutputBoundedAndKeepsBothEnds(t *testing.T) {
	out, code, err := runCommand(context.Background(), "/bin/sh", func(string) (string, bool) { return "", false },
		"-c", `echo START; head -c 40000000 /dev/zero | tr '\0' x; echo; echo END`)
	if err != nil || code != 0 {
		t.Fatalf("code %d err %v", code, err)
	}
	if len(out) > outputHeadBytes+outputTailBytes+256 {
		t.Errorf("kept %d bytes", len(out))
	}
	s := string(out)
	if !strings.HasPrefix(s, "START\n") || !strings.HasSuffix(s, "END\n") {
		t.Errorf("lost an end: %q ... %q", s[:16], s[len(s)-16:])
	}
	if !strings.Contains(s, "bytes of output dropped") {
		t.Error("no marker")
	}
}

func TestHostExec_OutputBoundedAndKeepsBothEnds(t *testing.T) {
	r := &hostRunner{env: []string{"PATH=/usr/bin:/bin"}}
	out, err := r.Exec(context.Background(), `echo START; head -c 40000000 /dev/zero | tr '\0' x; echo; echo END; exit 4`, "")
	if err != nil {
		t.Fatal(err)
	}
	if out.ExitCode != 4 {
		t.Errorf("exit %d", out.ExitCode)
	}
	if len(out.Combined) > outputHeadBytes+outputTailBytes+256 {
		t.Errorf("kept %d bytes", len(out.Combined))
	}
	s := string(out.Combined)
	if !strings.HasPrefix(s, "START\n") || !strings.HasSuffix(s, "END\n") || !strings.Contains(s, "bytes of output dropped") {
		t.Errorf("head %q tail %q", s[:16], s[len(s)-16:])
	}
}

func TestHostExec_InterleavesStdoutAndStderr(t *testing.T) {
	r := &hostRunner{env: []string{"PATH=/usr/bin:/bin"}}
	out, err := r.Exec(context.Background(), `echo a; echo b >&2; echo c`, "")
	if err != nil {
		t.Fatal(err)
	}
	if string(out.Combined) != "a\nb\nc\n" {
		t.Errorf("%q", out.Combined)
	}
}

// A daemon started without redirecting its output keeps the pipe open. The command
// is finished, so that is a success after the delay, not a hang until the timeout.
func TestHostExec_BackgroundHoldingPipe_ReturnsAndSucceeds(t *testing.T) {
	r := &hostRunner{env: []string{"PATH=/usr/bin:/bin"}}
	at := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// Short enough to end by itself soon after the test. Nothing here kills it.
	out, err := r.Exec(ctx, "sleep 12 & echo started", "")
	if err != nil {
		t.Fatal(err)
	}
	if took := time.Since(at); took > pipeWaitDelay+5*time.Second {
		t.Errorf("took %s", took)
	}
	if out.ExitCode != 0 || out.TimedOut || string(out.Combined) != "started\n" {
		t.Errorf("out %+v", out)
	}
}

func TestHostExec_TimeoutKillsWholeGroup(t *testing.T) {
	r := &hostRunner{env: []string{"PATH=/usr/bin:/bin"}}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	at := time.Now()
	out, err := r.Exec(ctx, `sleep 300; echo never`, "")
	if err != nil {
		t.Fatal(err)
	}
	if !out.TimedOut || out.ExitCode == 0 {
		t.Errorf("out %+v", out)
	}
	if took := time.Since(at); took > time.Second {
		t.Errorf("took %s: the sleep held the pipe", took)
	}
}

func TestBoundedOutput(t *testing.T) {
	newOut := func(h, tl int) *boundedOutput { return &boundedOutput{headMax: h, tailMax: tl} }
	t.Run("under_limit_is_verbatim", func(t *testing.T) {
		b := newOut(4, 4)
		b.Write([]byte("abc"))
		b.Write([]byte("de"))
		if string(b.Bytes()) != "abcde" {
			t.Errorf("%q", b.Bytes())
		}
	})
	t.Run("exactly_full_has_no_marker", func(t *testing.T) {
		b := newOut(2, 2)
		b.Write([]byte("abcd"))
		if string(b.Bytes()) != "abcd" {
			t.Errorf("%q", b.Bytes())
		}
	})
	t.Run("drops_middle_and_counts", func(t *testing.T) {
		b := newOut(2, 2)
		for _, c := range "abcdefgh" {
			if n, err := b.Write([]byte(string(c))); n != 1 || err != nil {
				t.Fatalf("n %d err %v", n, err)
			}
		}
		s := string(b.Bytes())
		if !strings.HasPrefix(s, "ab") || !strings.HasSuffix(s, "gh") || !strings.Contains(s, "4 bytes of output dropped") {
			t.Errorf("%q", s)
		}
	})
	t.Run("one_huge_write", func(t *testing.T) {
		b := newOut(2, 3)
		if n, _ := b.Write([]byte("0123456789")); n != 10 {
			t.Errorf("n = %d", n)
		}
		s := string(b.Bytes())
		if !strings.HasPrefix(s, "01") || !strings.HasSuffix(s, "789") || !strings.Contains(s, "5 bytes") {
			t.Errorf("%q", s)
		}
	})
	t.Run("ring_wraps_mid_write", func(t *testing.T) {
		b := newOut(1, 4)
		b.Write([]byte("0abcd"))
		b.Write([]byte("ef"))
		b.Write([]byte("ghi"))
		if s := string(b.Bytes()); !strings.HasPrefix(s, "0") || !strings.HasSuffix(s, "fghi") {
			t.Errorf("%q", s)
		}
	})
}

package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/ethanhinson/fuse/internal/agent"
	"github.com/ethanhinson/fuse/internal/config"
	"github.com/ethanhinson/fuse/internal/event"
	"github.com/ethanhinson/fuse/internal/model"
	"github.com/ethanhinson/fuse/internal/orchestrator"
	"github.com/ethanhinson/fuse/internal/permissions"
	"github.com/ethanhinson/fuse/internal/tools"
	"github.com/ethanhinson/fuse/internal/tui"
	"github.com/ethanhinson/fuse/sdk/fuse"
)

// orchestrate_cli.go: `fuse steer` and `fuse orchestrate`, the command-line way to steer
// a run that has no TUI - a one-shot run's control socket, a loop-serve-net server (a
// container, a compose stack, a Helm release), or anything else that speaks the
// fuse.loop.v1 wire. Both are thin clients: they talk the wire through the Go SDK.

// connFlags are the flags every remote-steering command shares.
type connFlags struct {
	socket string
	url    string
	token  string
	tenant string
	loop   string
}

func (c *connFlags) register(fs *flag.FlagSet) {
	fs.StringVar(&c.socket, "socket", "", "unix control socket of a run started with --control")
	fs.StringVar(&c.url, "url", "", "base URL of a fuse loop-serve-net server")
	fs.StringVar(&c.token, "token", os.Getenv("FUSE_TOKEN"), "bearer token for --url (default $FUSE_TOKEN)")
	fs.StringVar(&c.tenant, "tenant", "", "tenant for --url")
	fs.StringVar(&c.loop, "loop", "", "id of the loop to steer")
}

// client builds an SDK client for whichever transport the flags name.
func (c *connFlags) client() (*fuse.Client, error) {
	switch {
	case c.loop == "":
		return nil, fmt.Errorf("--loop is required")
	case c.socket != "" && c.url != "":
		return nil, fmt.Errorf("use either --socket or --url, not both")
	case c.socket != "":
		sock := c.socket
		hc := &http.Client{Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", sock)
			},
		}}
		return fuse.NewRemote("http://fuse.control", fuse.Credentials{Tenant: c.tenant}, fuse.WithHTTPClient(hc)), nil
	case c.url != "":
		return fuse.NewRemote(c.url, fuse.Credentials{Token: c.token, Tenant: c.tenant}), nil
	}
	return nil, fmt.Errorf("one of --socket or --url is required")
}

// runSteer sends one message to a running loop, optionally interrupting what it is doing.
func runSteer(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("fuse steer", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var cf connFlags
	cf.register(fs)
	interrupt := fs.Bool("interrupt", false, "stop the agent's current model call or tool run so the message lands now")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	msg := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if msg == "" {
		fmt.Fprintln(stderr, "usage: fuse steer (--socket PATH | --url URL) --loop ID [--interrupt] \"message\"")
		return 2
	}
	client, err := cf.client()
	if err != nil {
		fmt.Fprintf(stderr, "steer: %v\n", err)
		return 2
	}
	if err := client.SendWith(context.Background(), cf.loop, msg, fuse.SendOptions{Interrupt: *interrupt}); err != nil {
		fmt.Fprintf(stderr, "steer: %v\n", err)
		return 1
	}
	if *interrupt {
		fmt.Fprintln(stdout, "sent (interrupting)")
	} else {
		fmt.Fprintln(stdout, "sent")
	}
	return 0
}

// runOrchestrate is a line-by-line chat with an orchestrator that watches and steers a
// running loop. The orchestrator model runs HERE, on this machine's gateway config; it
// reaches the loop only through the wire, so the same command works against a one-shot
// run's socket and a hosted server alike.
func runOrchestrate(args []string, cfg config.Config, reg *model.Registry, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("fuse orchestrate", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var cf connFlags
	cf.register(fs)
	modelFlag := fs.String("model", "", "model alias for the orchestrator (default: orchestrator.model, else models.default)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	client, err := cf.client()
	if err != nil {
		fmt.Fprintf(stderr, "orchestrate: %v\n", err)
		return 2
	}
	alias := *modelFlag
	if alias == "" {
		alias = cfg.Orchestrator.Model
	}
	if alias == "" {
		alias = reg.Default
	}
	if _, err := reg.Resolve(alias); err != nil {
		fmt.Fprintf(stderr, "orchestrate: model %q: %v\n", alias, err)
		return 1
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	rateGate := sessionRateGate(cfg)
	out := make(chan tea.Msg, 64)
	target := newWireTarget(ctx, client, cf.loop)
	orch := newShellOrchestrator(ctx, out, target, func(r agent.Renderer, orchTools *tools.Registry) (*agent.Agent, error) {
		// No human answers this orchestrator: a policy stand-in that denies
		// every prompt, so its denials record decided_by: policy.
		deny := permissions.Policy(func(context.Context, permissions.ApprovalRequest) (bool, bool, error) { return false, false, nil })
		return buildChildAgent(cfg, reg, alias, r, orchestrator.SystemPrompt(), orchTools, deny, nil,
			"orchestrator", permissions.NewSessionMode(permissions.ModeSmart), true, rateGate, nil)
	})

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case m := <-out:
				printOrchestratorMsg(stdout, m)
			case <-ctx.Done():
				return
			}
		}
	}()

	fmt.Fprintf(stderr, "orchestrator (%s) watching loop %s. Type a message; empty line or /exit to leave.\n", alias, cf.loop)
	sc := bufio.NewScanner(stdin)
	for {
		fmt.Fprint(stderr, "you> ")
		if !sc.Scan() {
			break
		}
		line := strings.TrimSpace(sc.Text())
		if line == "" || line == "/exit" {
			break
		}
		orch.Say(line)
		if err := orch.WaitIdle(ctx); err != nil {
			break
		}
		drainOrchestratorMsgs(out, stdout)
	}
	stop()
	wg.Wait()
	return 0
}

// drainOrchestratorMsgs prints whatever the printer goroutine has not yet taken, so the
// answer is on screen before the next prompt.
func drainOrchestratorMsgs(out <-chan tea.Msg, w io.Writer) {
	for {
		select {
		case m := <-out:
			printOrchestratorMsg(w, m)
		default:
			return
		}
	}
}

func printOrchestratorMsg(w io.Writer, m tea.Msg) {
	switch v := m.(type) {
	case tui.OrchReplyMsg:
		fmt.Fprintf(w, "orchestrator: %s\n", v.Text)
	case tui.OrchActionMsg:
		fmt.Fprintf(w, "  > %s\n", v.Text)
	case tui.OrchErrMsg:
		fmt.Fprintf(w, "  ! %s\n", v.Err)
	}
}

// wireTarget is an orchestrator.Target over the SDK: it follows the loop's event stream
// and steers through Send, so the orchestrator reaches a remote loop the same way any
// client does.
//
// It starts following the stream when it is built, so the history is already arriving
// by the time the orchestrator first looks. The wire has no "history finished" marker
// (Observe replays then tails on one stream), so the first read waits for the stream to
// go quiet before answering; later reads just take what has arrived.
type wireTarget struct {
	ctx    context.Context
	client *fuse.Client
	loop   string

	mu      sync.Mutex
	err     error
	buf     []event.Event
	arrived chan struct{} // signalled (coalesced) whenever an event is buffered
	settled bool
}

func newWireTarget(ctx context.Context, c *fuse.Client, loop string) *wireTarget {
	t := &wireTarget{ctx: ctx, client: c, loop: loop, arrived: make(chan struct{}, 1)}
	t.follow()
	return t
}

const (
	// maxWireBuf bounds the events held between two reads of the watcher.
	maxWireBuf = 4000
	// settleFirst is how long the first read waits for any history to show up.
	settleFirst = time.Second
	// settleQuiet is how long the stream must be silent before history counts as done.
	settleQuiet = 150 * time.Millisecond
	// settleMax caps the whole wait, so a busy stream cannot hold the first read forever.
	settleMax = 2 * time.Second
)

func (t *wireTarget) follow() {
	ch, unsub, err := t.client.Observe(t.ctx, t.loop, 0)
	if err != nil {
		t.err = err
		return
	}
	go func() {
		defer unsub()
		for ev := range ch {
			t.mu.Lock()
			t.buf = append(t.buf, ev)
			if over := len(t.buf) - maxWireBuf; over > 0 {
				t.buf = append([]event.Event(nil), t.buf[over:]...)
			}
			t.mu.Unlock()
			select {
			case t.arrived <- struct{}{}:
			default:
			}
		}
	}()
}

// settle waits, once, for the replayed history to arrive: first for any event (a loop
// with no events yet never sends one, hence the short cap), then for the stream to go
// quiet.
func (t *wireTarget) settle(ctx context.Context) {
	deadline := time.NewTimer(settleMax)
	defer deadline.Stop()
	wait := settleFirst
	for {
		quiet := time.NewTimer(wait)
		select {
		case <-t.arrived:
			quiet.Stop()
			wait = settleQuiet
		case <-quiet.C:
			return
		case <-deadline.C:
			quiet.Stop()
			return
		case <-ctx.Done():
			quiet.Stop()
			return
		}
	}
}

func (t *wireTarget) Events(ctx context.Context, after event.Seq) ([]event.Event, error) {
	t.mu.Lock()
	first := !t.settled
	t.settled = true
	err := t.err
	t.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if first {
		t.settle(ctx)
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	// The watcher only ever moves forward, so what it has already seen can go.
	keep := t.buf[:0]
	for _, e := range t.buf {
		if e.Seq > after {
			keep = append(keep, e)
		}
	}
	t.buf = keep
	return append([]event.Event(nil), keep...), nil
}

func (t *wireTarget) Steer(ctx context.Context, text string, interrupt bool) error {
	return t.client.SendWith(ctx, t.loop, text, fuse.SendOptions{Interrupt: interrupt, Source: fuse.SourceOrchestrator})
}

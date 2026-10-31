package main

import (
	"context"
	"errors"
	"math"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/ethanhinson/fuse/harness"
	harnessv1 "github.com/ethanhinson/fuse/harness/v1"
	"github.com/ethanhinson/fuse/internal/capabilities"
	"github.com/ethanhinson/fuse/internal/event"
	"github.com/ethanhinson/fuse/internal/loopconnect"
	loopv1 "github.com/ethanhinson/fuse/internal/loopwire/v1"
	"github.com/ethanhinson/fuse/internal/loopwire/v1/loopv1connect"
	"github.com/ethanhinson/fuse/internal/runtime"
)

// Some ceilings set, some unset, an operator workflow, a hidden model, and two
// secrets: one for the default tenant and one for acme.
const capsOperator = `
models:
  default: fast
  fast: {id: cloud/fast, label: Fast, tags: [tools]}
  house: {id: cloud/house}
  quiet: {id: cloud/quiet, advertise: false}
max_turns: 2
agents: {queue_bound: 4, tool_timeout_seconds: 120}
throughput: {requests_per_minute: 6000, tokens_per_minute: 900000}
pipeline: {synthesis: {max_steps: 20, max_fanout: 8}}
workflows:
  triage: {skill: notes, pool: {concurrent: 4, total: 16}}
loop_server:
  secrets:
    - {name: search-key, env: FUSE_TEST_SEARCH_KEY}
    - {name: acme-key, env: FUSE_TEST_ACME_KEY, tenants: [acme]}
`

// capsHost is the production loop-server runtime and its harness host over a
// scripted gateway, with capsOperator as the operator config.
func capsHost(t *testing.T) (runtime.Runtime, *loopServerHost) {
	t.Helper()
	t.Setenv("FUSE_TEST_SEARCH_KEY", "search-value")
	t.Setenv("FUSE_TEST_ACME_KEY", "acme-value")
	rt, _, host := harnessServer(t, capsOperator)
	return rt, host
}

// setSpecNumber sets the numeric field at a ceiling path (dotted, with
// workflows["name"] segments), creating parents.
func setSpecNumber(t *testing.T, spec *harnessv1.HarnessSpec, path string, v float64) {
	t.Helper()
	m := spec.ProtoReflect()
	segs := strings.Split(path, ".")
	for i, seg := range segs {
		name, key, isMap := seg, "", false
		if j := strings.Index(seg, "["); j >= 0 {
			k, err := strconv.Unquote(seg[j+1 : len(seg)-1])
			if err != nil {
				t.Fatal(err)
			}
			name, key, isMap = seg[:j], k, true
		}
		fd := m.Descriptor().Fields().ByName(protoreflect.Name(name))
		if fd == nil {
			t.Fatalf("path %q: no field %q", path, name)
		}
		switch {
		case isMap:
			m = m.Mutable(fd).Map().Mutable(protoreflect.ValueOfString(key).MapKey()).Message()
		case i < len(segs)-1:
			m = m.Mutable(fd).Message()
		case fd.Kind() == protoreflect.DoubleKind:
			m.Set(fd, protoreflect.ValueOfFloat64(v))
		case fd.Kind() == protoreflect.Uint32Kind:
			m.Set(fd, protoreflect.ValueOfUint32(uint32(v)))
		default:
			t.Fatalf("path %q: kind %v", path, fd.Kind())
		}
	}
}

// loosenEveryNumber sets every singular uint32 and double field reachable from
// m through singular message fields to a value above any ceiling, without
// reading which fields are tighten-only. Maps, lists and oneof members are
// left alone, and a message type already on the path is not entered again.
func loosenEveryNumber(m protoreflect.Message, onPath map[protoreflect.FullName]bool) {
	md := m.Descriptor()
	if onPath[md.FullName()] {
		return
	}
	onPath[md.FullName()] = true
	defer delete(onPath, md.FullName())
	for i := 0; i < md.Fields().Len(); i++ {
		fd := md.Fields().Get(i)
		if fd.IsMap() || fd.IsList() {
			continue
		}
		if o := fd.ContainingOneof(); o != nil && !o.IsSynthetic() {
			continue // setting one member would clear its siblings
		}
		switch {
		case fd.Message() != nil:
			loosenEveryNumber(m.Mutable(fd).Message(), onPath)
		case fd.Kind() == protoreflect.Uint32Kind:
			m.Set(fd, protoreflect.ValueOfUint32(1_000_000_000))
		case fd.Kind() == protoreflect.DoubleKind:
			m.Set(fd, protoreflect.ValueOfFloat64(1e9))
		}
	}
}

// violationAt returns err's violation at path, failing when err is not a
// ValidationError or has none there.
func violationAt(t *testing.T, err error, path string) harness.Violation {
	t.Helper()
	var ve *harness.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("err = %v, want a violation at %s", err, path)
	}
	for _, v := range ve.Violations {
		if v.FieldPath == path {
			return v
		}
	}
	t.Fatalf("violations %v have none at %s", ve.Violations, path)
	return harness.Violation{}
}

// TestCapabilitiesAgreeWithStartLoop is the golden consistency test: the
// production host's advertisement against what the production runtime's
// StartLoop does. A spec built only from what is advertised starts; stepping
// past any one advertised limit is refused at that limit's field path.
func TestCapabilitiesAgreeWithStartLoop(t *testing.T) {
	rt, host := capsHost(t)
	ctx := context.Background()
	c, err := host.Capabilities(ctx, event.DefaultTenant)
	if err != nil || len(c.SecretRefs) == 0 {
		t.Fatalf("Capabilities = %+v, %v", c, err)
	}
	// The pre-flighted spec: every operator-set ceiling tightened, every
	// advertised skill named, an advertised secret, an advertised model.
	turns := uint32(1)
	base := &harnessv1.HarnessSpec{MaxTurns: &turns, Models: &harnessv1.Models{DefaultAlias: "house"},
		Research: &harnessv1.Research{Provider: harnessv1.ResearchProvider_RESEARCH_PROVIDER_CUSTOM,
			Custom: &harnessv1.CustomSearchProvider{Url: "https://search.example.com/search", Headers: map[string]*harnessv1.HeaderValue{
				"X-Api-Key": {Value: &harnessv1.HeaderValue_SecretRef{SecretRef: c.SecretRefs[0]}}}}}}
	// A spec may set a tighten-only field only to a positive value strictly
	// below its ceiling, so a ceiling of 1 on a whole-number field (the
	// built-in research workflow's pool.max_depth) admits no value and stays
	// unset here, exactly as an unset ceiling does.
	tightened := 0
	for _, ce := range c.Ceilings {
		if v := math.Floor(ce.Value / 2); ce.OperatorSet && v >= 1 {
			setSpecNumber(t, base, ce.FieldPath, v)
			tightened++
		}
	}
	if tightened == 0 {
		t.Fatalf("no ceiling was tightened: %+v", c.Ceilings)
	}
	for _, s := range c.Skills {
		base.Skills = append(base.Skills, &harnessv1.SkillRef{Name: s.Name})
	}
	clone := func() *harnessv1.HarnessSpec { return proto.Clone(base).(*harnessv1.HarnessSpec) }
	start := func(spec *harnessv1.HarnessSpec) error {
		_, err := rt.StartLoop(ctx, runtime.LoopConfig{Task: "go", Harness: spec})
		return err
	}

	h, err := rt.StartLoop(ctx, runtime.LoopConfig{Task: "go", Harness: clone()})
	if err != nil {
		t.Fatalf("StartLoop refused the pre-flighted spec: %v", err)
	}
	_, _ = h.Wait()

	for _, ce := range c.Ceilings {
		spec := clone()
		over := 1.0 // unset by the operator: any positive value is refused
		if ce.OperatorSet {
			over = ce.Value + 1
		}
		setSpecNumber(t, spec, ce.FieldPath, over)
		if v := violationAt(t, start(spec), ce.FieldPath); v.RuleID != harness.RuleTightenOnly {
			t.Errorf("%s: rule %s", ce.FieldPath, v.RuleID)
		}
		if !ce.OperatorSet {
			continue
		}
		// The ceiling itself is not strictly below it: also refused.
		spec = clone()
		setSpecNumber(t, spec, ce.FieldPath, ce.Value)
		if v := violationAt(t, start(spec), ce.FieldPath); v.RuleID != harness.RuleTightenOnly {
			t.Errorf("%s at its ceiling: rule %s", ce.FieldPath, v.RuleID)
		}
	}

	// Completeness: a spec that loosens every numeric field, including every
	// operator-defined workflow's, is refused as tighten-only at exactly the
	// advertised ceiling paths - no fewer, no more.
	loose := clone()
	loosenEveryNumber(loose.ProtoReflect(), map[protoreflect.FullName]bool{})
	for name := range host.operator.cfg.Workflows {
		wf := loose.ProtoReflect()
		fd := wf.Descriptor().Fields().ByName("workflows")
		loosenEveryNumber(wf.Mutable(fd).Map().Mutable(protoreflect.ValueOfString(name).MapKey()).Message(),
			map[protoreflect.FullName]bool{})
	}
	var refusedAt, advertisedAt []string
	var ve *harness.ValidationError
	if !errors.As(start(loose), &ve) {
		t.Fatal("StartLoop accepted a spec that loosens every limit")
	}
	for _, v := range ve.Violations {
		if v.RuleID == harness.RuleTightenOnly {
			refusedAt = append(refusedAt, v.FieldPath)
		}
	}
	for _, ce := range c.Ceilings {
		advertisedAt = append(advertisedAt, ce.FieldPath)
	}
	slices.Sort(refusedAt)
	slices.Sort(advertisedAt)
	if !slices.Equal(refusedAt, advertisedAt) {
		t.Errorf("tighten-only refusals at\n %v\nadvertised ceilings at\n %v", refusedAt, advertisedAt)
	}

	spec := clone()
	spec.Skills = []*harnessv1.SkillRef{{Name: "not-installed"}}
	violationAt(t, start(spec), "skills[0].name")

	spec = clone()
	spec.Research.Custom.Headers["X-Api-Key"] = &harnessv1.HeaderValue{Value: &harnessv1.HeaderValue_SecretRef{SecretRef: "acme-key"}}
	violationAt(t, start(spec), `research.custom.headers["X-Api-Key"].secret_ref`)

	advertised := map[harnessv1.McpAuthType]bool{}
	for _, k := range c.SpecMCPAuthKinds {
		advertised[k] = true
	}
	vals := harnessv1.McpAuthType(0).Descriptor().Values()
	for i := 0; i < vals.Len(); i++ {
		k := harnessv1.McpAuthType(vals.Get(i).Number())
		spec := clone()
		auth := &harnessv1.McpAuth{Type: k}
		if k == harnessv1.McpAuthType_MCP_AUTH_TYPE_BEARER || k == harnessv1.McpAuthType_MCP_AUTH_TYPE_STATIC {
			auth.ClientSecretRef = c.SecretRefs[0]
		}
		spec.McpServers = []*harnessv1.McpServer{{Name: "s", Transport: harnessv1.McpTransport_MCP_TRANSPORT_HTTP,
			Url: "https://mcp.example.com", Audience: "https://mcp.example.com", Auth: auth}}
		if advertised[k] {
			// Acceptance through the resolver StartLoop calls, without building
			// a loop that would dial the MCP server.
			if _, err := host.ResolveHarness(ctx, event.DefaultTenant, spec); err != nil {
				t.Errorf("advertised auth %v refused: %v", k, err)
			}
			continue
		}
		if v := violationAt(t, start(spec), "mcp_servers[0].auth.type"); v.RuleID != harness.RuleOperatorOnly {
			t.Errorf("%v: rule %s", k, v.RuleID)
		}
	}
}

// TestLoopServerHostAdvertisesItsCapabilities pins what the production host
// lists: routable, non-hidden models with their operator metadata, the
// installed skills, the tenant's resolvable secret names, and the build.
func TestLoopServerHostAdvertisesItsCapabilities(t *testing.T) {
	_, host := capsHost(t)
	ctx := context.Background()
	c, err := host.Capabilities(ctx, event.DefaultTenant)
	if err != nil {
		t.Fatal(err)
	}

	models := map[string]capabilities.Model{}
	for _, m := range c.Models {
		models[m.Alias] = m
	}
	fast, ok := models["fast"]
	if !ok {
		t.Fatalf("models %+v do not list fast", c.Models)
	}
	if !fast.IsDefault || fast.Label != "Fast" || !fast.ContextWindowIsDefault || fast.GatewayID != "cloud/fast" {
		t.Errorf("fast = %+v, want the default, labelled Fast, on the default context window", fast)
	}
	if !slices.Equal(fast.Tags, []string{"tools"}) {
		t.Errorf("fast tags = %v, want [tools]", fast.Tags)
	}
	if _, ok := models["house"]; !ok {
		t.Errorf("models %+v do not list house", c.Models)
	}
	for _, hidden := range []string{"quiet", "claude-max"} {
		if _, ok := models[hidden]; ok {
			t.Errorf("models list %s, which a server-mode loop cannot run", hidden)
		}
	}

	var names []string
	for _, s := range c.Skills {
		names = append(names, s.Name)
	}
	for _, want := range []string{"notes", "research"} {
		if !slices.Contains(names, want) {
			t.Errorf("skills %v do not include %s", names, want)
		}
	}

	if !slices.Equal(c.SecretRefs, []string{"search-key"}) {
		t.Errorf("default tenant secret refs = %v, want [search-key]", c.SecretRefs)
	}
	acme, err := host.Capabilities(ctx, "acme")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(acme.SecretRefs, []string{"acme-key"}) {
		t.Errorf("acme secret refs = %v, want [acme-key]", acme.SecretRefs)
	}

	if c.Server.DigestAlgorithm != harness.DigestAlgorithm || c.Server.Version == "" {
		t.Errorf("server = %+v, want digest %s and a version", c.Server, harness.DigestAlgorithm)
	}
	if len(c.Ceilings) == 0 || len(c.SpecMCPAuthKinds) == 0 {
		t.Errorf("ceilings %v or spec MCP auth kinds %v are empty", c.Ceilings, c.SpecMCPAuthKinds)
	}
}

// TestLoopCapabilitySourceFromProductionDeps proves the composition root takes
// its GetCapabilities source from the deps' harness host, and refuses deps that
// carry none instead of serving an RPC that always answers Unimplemented.
func TestLoopCapabilitySourceFromProductionDeps(t *testing.T) {
	_, host := capsHost(t)
	src, err := loopCapabilitySource(runtime.Deps{Harness: host})
	if err != nil {
		t.Fatalf("loopCapabilitySource(host) error = %v", err)
	}
	if src != loopconnect.CapabilitySource(host) {
		t.Errorf("loopCapabilitySource returned %v, want the harness host", src)
	}
	if _, err := loopCapabilitySource(runtime.Deps{}); err == nil {
		t.Error("loopCapabilitySource(empty deps) = nil error, want a refusal")
	}
}

// TestServeNetMountsTheCapabilitySource proves serveNetWithOptions hands its
// capability source to the Connect handler, so GetCapabilities answers over the
// wire with the host's models and ceilings.
func TestServeNetMountsTheCapabilitySource(t *testing.T) {
	rt, host := capsHost(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- serveNetWithOptions(ctx, ln, rt, nil, nil, nil, nil, serveNetOptions{capabilities: host})
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})

	client := loopv1connect.NewLoopServiceClient(http.DefaultClient, "http://"+ln.Addr().String())
	req := &loopv1.GetCapabilitiesRequest{Tenant: string(event.DefaultTenant)}
	var resp *connect.Response[loopv1.GetCapabilitiesResponse]
	deadline := time.Now().Add(2 * time.Second)
	for {
		resp, err = client.GetCapabilities(ctx, connect.NewRequest(req))
		if err == nil || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("GetCapabilities: %v", err)
	}
	if len(resp.Msg.Models) == 0 || len(resp.Msg.Ceilings) == 0 {
		t.Errorf("GetCapabilities models %v or ceilings %v are empty", resp.Msg.Models, resp.Msg.Ceilings)
	}
}

package loopv1_test

import (
	"context"
	"net/http/httptest"
	"testing"

	"connectrpc.com/connect"
	loopv1 "github.com/ethanhinson/fuse/internal/loopwire/v1"
	"github.com/ethanhinson/fuse/internal/loopwire/v1/loopv1connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// TestGeneratedStubsCompileAndShape proves the committed proto stubs build and the
// fuse.loop.v1 service shape is present: every request/response message constructs,
// and the connect client/handler interfaces exist with the three RPCs (StartLoop,
// Send unary; Observe server-streaming). It is the Task 1 build/shape guard — if the
// contract drifts or regeneration produces a different surface, this stops compiling.
func TestGeneratedStubsCompileAndShape(t *testing.T) {
	// Construct each message so the generated Go types are exercised, not just named.
	_ = &loopv1.StartLoopRequest{Task: "t", Model: "m", Tenant: "tn", Interactive: true}
	_ = &loopv1.StartLoopResponse{LoopId: "loop-1"}
	_ = &loopv1.SendRequest{LoopId: "loop-1", Input: "hi", Tenant: "tn"}
	_ = &loopv1.SendResponse{}
	_ = &loopv1.ObserveRequest{LoopId: "loop-1", FromSeq: 7, Tenant: "tn"}
	_ = &loopv1.ObserveEvent{
		Event:     &loopv1.Event{Seq: 1, NodeId: "n", Kind: "turn.start", Payload: []byte(`{}`)},
		Gap:       true,
		Keepalive: false,
	}

	// The handler interface is satisfiable (compile-time) by the generated
	// Unimplemented base, and the mount function returns a path + handler.
	var _ loopv1connect.LoopServiceHandler = loopv1connect.UnimplementedLoopServiceHandler{}
	path, h := loopv1connect.NewLoopServiceHandler(loopv1connect.UnimplementedLoopServiceHandler{})
	if path == "" || h == nil {
		t.Fatal("NewLoopServiceHandler returned empty path/handler")
	}

	// The client interface is constructible against a live handler; the three RPCs
	// are reachable (Unimplemented returns CodeUnimplemented — proving wiring, not
	// behavior).
	srv := httptest.NewServer(h)
	defer srv.Close()
	client := loopv1connect.NewLoopServiceClient(srv.Client(), srv.URL)

	if _, err := client.StartLoop(context.Background(), connect.NewRequest(&loopv1.StartLoopRequest{})); connect.CodeOf(err) != connect.CodeUnimplemented {
		t.Fatalf("StartLoop code = %v, want Unimplemented (wiring check)", connect.CodeOf(err))
	}
	if _, err := client.Send(context.Background(), connect.NewRequest(&loopv1.SendRequest{})); connect.CodeOf(err) != connect.CodeUnimplemented {
		t.Fatalf("Send code = %v, want Unimplemented", connect.CodeOf(err))
	}
	stream, err := client.Observe(context.Background(), connect.NewRequest(&loopv1.ObserveRequest{}))
	if err != nil {
		t.Fatalf("Observe open: %v", err)
	}
	if stream.Receive() {
		t.Fatal("Observe unexpectedly received a frame from Unimplemented handler")
	}
	if connect.CodeOf(stream.Err()) != connect.CodeUnimplemented {
		t.Fatalf("Observe err code = %v, want Unimplemented", connect.CodeOf(stream.Err()))
	}
}

// TestStartLoopRequestCarriesSecrets pins the loop-scoped secrets wire contract:
// StartLoopRequest.secrets (field 7) round-trips repeated LoopSecret entries whose
// name/value/hosts are fields 1/2/3.
func TestStartLoopRequestCarriesSecrets(t *testing.T) {
	in := &loopv1.StartLoopRequest{
		Task: "t",
		Secrets: []*loopv1.LoopSecret{
			{Name: "my-token", Value: "v", Hosts: []string{"mcp.acme.test"}},
		},
	}
	b, err := proto.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	out := &loopv1.StartLoopRequest{}
	if err := proto.Unmarshal(b, out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !proto.Equal(in, out) {
		// Never format the messages: they carry a secret value in clear.
		t.Fatalf("round trip mismatch: task %q/%q, %d/%d secrets", out.GetTask(), in.GetTask(), len(out.GetSecrets()), len(in.GetSecrets()))
	}

	if n := (&loopv1.StartLoopRequest{}).ProtoReflect().Descriptor().Fields().ByName("secrets").Number(); n != 7 {
		t.Fatalf("StartLoopRequest.secrets field number = %d, want 7", n)
	}
	fields := (&loopv1.LoopSecret{}).ProtoReflect().Descriptor().Fields()
	for name, want := range map[string]int32{"name": 1, "value": 2, "hosts": 3} {
		f := fields.ByName(protoreflect.Name(name))
		if f == nil {
			t.Fatalf("LoopSecret has no field %q", name)
		}
		if got := int32(f.Number()); got != want {
			t.Fatalf("LoopSecret.%s field number = %d, want %d", name, got, want)
		}
	}
}

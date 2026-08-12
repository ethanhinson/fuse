package event

import (
	"strings"
	"testing"
)

func TestEventTraceCarrierRoundTrip(t *testing.T) {
	e := Event{Seq: 9, Kind: KindTurnStart, Trace: &TraceCarrier{
		TraceParent: "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01",
		TraceState:  "acme=tenant",
	}}

	line, err := e.MarshalJSONL()
	if err != nil {
		t.Fatalf("MarshalJSONL: %v", err)
	}
	got, err := ParseEvent(line)
	if err != nil {
		t.Fatalf("ParseEvent: %v", err)
	}
	if got.Trace == nil || *got.Trace != *e.Trace {
		t.Fatalf("trace carrier = %#v, want %#v", got.Trace, e.Trace)
	}
}

func TestEventTraceCarrierAbsentPreservesWireShape(t *testing.T) {
	line, err := (Event{Seq: 9, Kind: KindTurnStart}).MarshalJSONL()
	if err != nil {
		t.Fatalf("MarshalJSONL: %v", err)
	}
	if strings.Contains(string(line), "trace") {
		t.Fatalf("absent trace carrier changed wire format: %s", line)
	}
	got, err := ParseEvent(line)
	if err != nil {
		t.Fatalf("ParseEvent: %v", err)
	}
	if got.Trace != nil {
		t.Fatalf("absent trace carrier decoded as %#v", got.Trace)
	}
}

func TestEventTraceCarrierRejectsInvalidW3CValues(t *testing.T) {
	bad := Event{Trace: &TraceCarrier{TraceParent: "00-not-a-trace-id-00f067aa0ba902b7-01"}}
	if _, err := bad.MarshalJSONL(); err == nil {
		t.Fatal("MarshalJSONL accepted malformed traceparent")
	}

	line := []byte(`{"seq":1,"kind":"turn.start","trace":{"traceparent":"00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01","tracestate":"bad value"}}`)
	if _, err := ParseEvent(line); err == nil {
		t.Fatal("ParseEvent accepted malformed tracestate")
	}
}

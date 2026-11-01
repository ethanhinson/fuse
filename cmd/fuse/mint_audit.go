package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"

	"github.com/ethanhinson/fuse/internal/event"
	"github.com/ethanhinson/fuse/internal/toolidentity"
	"github.com/ethanhinson/fuse/internal/tools"
)

// loopMintAudit is the tool-identity broker's audit sink on the loop-server
// host: it appends one tool.identity.minted event to the stream of the loop a
// delegated token was minted for. Loops are bound in the loop factory's
// BuildAgent (the first place holding the loop's own store) and unbound in
// LoopTeardown (which also runs on a StartLoop early return). A record for a
// loop this host is not running, or one without a loop, is dropped.
//
// Only oauth-tier records are recorded: the static tier mints nothing. The
// payload is the subject, audience, joined scopes and loop id, never the token.
type loopMintAudit struct {
	byLoop sync.Map // loop id -> *loopMintBinding
	byReg  sync.Map // *tools.Registry -> *loopMintBinding
}

// loopMintBinding is one loop's registration. Both maps hold the same pointer,
// so unbind removes a loop id only while it still belongs to that registry.
type loopMintBinding struct {
	loopID string
	store  event.EventStore
}

var _ toolidentity.AuditSink = (*loopMintAudit)(nil)

func newLoopMintAudit() *loopMintAudit { return &loopMintAudit{} }

// bind routes mints for loopID to store until reg is unbound.
func (a *loopMintAudit) bind(reg *tools.Registry, loopID string, store event.EventStore) {
	if a == nil || reg == nil || loopID == "" || store == nil {
		return
	}
	b := &loopMintBinding{loopID: loopID, store: store}
	a.byLoop.Store(loopID, b)
	a.byReg.Store(reg, b)
}

// unbind drops the binding made for reg. It is safe for a registry that was
// never bound (a StartLoop that failed before BuildAgent). When the loop id has
// since been bound by another registry, that newer binding is kept.
func (a *loopMintAudit) unbind(reg *tools.Registry) {
	if a == nil || reg == nil {
		return
	}
	if v, ok := a.byReg.LoadAndDelete(reg); ok {
		b := v.(*loopMintBinding)
		a.byLoop.CompareAndDelete(b.loopID, b)
	}
}

// Record implements toolidentity.AuditSink. It has no error path: a failure to
// record is logged to stderr and never fails the tool call that minted.
func (a *loopMintAudit) Record(r toolidentity.AuditRecord) {
	if a == nil || r.Tier != "oauth" || r.Loop == "" {
		return
	}
	v, ok := a.byLoop.Load(r.Loop)
	if !ok {
		return
	}
	store := v.(*loopMintBinding).store
	payload, err := json.Marshal(event.ToolIdentityMintedPayload{
		Subject:  r.Subject,
		Audience: r.Audience,
		Scope:    strings.Join(r.Scopes, " "),
		LoopID:   r.Loop,
	})
	if err == nil {
		err = store.Append(event.Event{NodeID: r.Loop, Kind: event.KindToolIdentityMinted, Payload: payload})
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "loop-server: record tool.identity.minted for %s: %v\n", r.Loop, err)
	}
}

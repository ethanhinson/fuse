package sandbox

// WHY THIS FILE EXISTS.
//
// No microVM handler ships in this change. This file is SEAM VALIDATION, not a
// handler: it proves that Handler, Runner, and Env already accommodate a
// hardware-VM isolation mechanism with NO new method and NO widened type,
// before anyone commits to building one. See ADR-0044's 2026-08-16 Update
// ("microVM (Firecracker, Cloud Hypervisor, Kata-as-VM) — anticipated in-seam
// handler, same seam"), which also locks in the fail-closed requirement this
// file asserts at the type level: "a kvm-absent host MUST fail-CLOSED (refuse
// to run) — it MUST NOT degrade to the host/no-container off-switch", because
// that would be fail-open.
//
// microvmStub is deliberately minimal — it never spawns a guest, never touches
// /dev/kvm for real, and is never wired into Service or any composition root.
// Its only job is to type-check against Handler/Runner and to encode the
// fail-closed shape.
//
// THE MICROVM BOUNDARY CONTRACT (recorded, not built).
//
// This change (#0064) builds egress control for the container backend only.
// When a real microVM handler is eventually built behind this seam, its
// isolation boundary is NOT the same mechanism as the container backend's
// (--network none + host-side proxy + nftables on the host's own interface),
// and per ADR-0044 it MUST provide the following, all as guest/host-boundary
// properties rather than in-process policy:
//
//   - No-NIC floor: like the container's --network none, the guest's default
//     posture has no network interface at all. Absence of a NIC is the
//     off-switch, not an in-guest firewall rule the guest could see or flip.
//   - Host-side tap device: when egress IS declared, the host attaches a tap
//     device to the guest and enforces the allowlist from the host side —
//     the guest never holds the policy or the enforcement point, mirroring
//     why the container backend's proxy lives on the host, not in the
//     container's netns.
//   - nftables for declared egress: the host enforces the declared
//     allowlist via nftables rules on the tap device, the same primitive
//     class this change's container floor uses, not a bespoke in-guest
//     mechanism.
//   - env-scrub re-implemented as guest-init: this change's environment
//     scrubbing (stripping ambient credentials before a process starts) runs
//     as a guest-init step for a microVM, since there is no shared host
//     process environment to scrub from the outside — the guest boots into
//     an already-scrubbed environment rather than being scrubbed after the
//     fact.
//   - Per-principal snapshot/pool reset: a microVM handler reclaims a guest
//     between principals by resetting to a clean snapshot or pool image,
//     the VM-level equivalent of this change's per-principal proxy/runner
//     teardown — state never survives across principals.
//
// None of the above is implemented by this file or anywhere else in this
// change. It is recorded here, beside the seam-conformance stub it will
// eventually replace, so the contract survives until #0064's successor picks
// up the microVM handler.

import (
	"context"
	"errors"
	"testing"

	"github.com/ethanhinson/fuse/internal/loopauth"
)

// errMicrovmNotBuilt is what a future microVM handler's Acquire returns until
// it is actually implemented. It exists here only so the stub and its test can
// assert a stable, meaningful error rather than an arbitrary sentinel.
var errMicrovmNotBuilt = errors.New("microvm handler not built")

// microvmStub is a placeholder Handler standing in for a future Firecracker /
// Cloud Hypervisor / Kata-as-VM handler. kvmPresent is injected so the test
// controls kvm availability without touching the real filesystem.
type microvmStub struct {
	kvmPresent func() bool
}

// Compile-time seam-conformance assertions: the seam needs no new method and
// no widened type to accommodate a hardware-VM mechanism.
var _ Handler = (*microvmStub)(nil)
var _ Runner = (*microvmRunnerStub)(nil)

// Name reports the bounded handler identifier a real implementation would use.
func (*microvmStub) Name() string { return "microvm" }

// Acquire refuses unconditionally today (no handler is built), and — the
// load-bearing case — refuses when /dev/kvm is absent rather than ever
// returning something that could be mistaken for a host Runner.
func (m *microvmStub) Acquire(_ context.Context, _ loopauth.Principal, _ Env) (Runner, error) {
	if m.kvmPresent != nil && !m.kvmPresent() {
		// Fail CLOSED: per ADR-0044's Update, a kvm-absent host must refuse,
		// never degrade to the host/no-container off-switch.
		return nil, errMicrovmNotBuilt
	}
	return nil, errMicrovmNotBuilt
}

// microvmRunnerStub is a stub Runner satisfying the seam. It is never actually
// returned by microvmStub.Acquire in this file — its only purpose is to prove
// Runner accommodates a VM-shaped execution context with no widening.
type microvmRunnerStub struct{}

func (*microvmRunnerStub) Exec(context.Context, string, string) (Output, error) {
	return Output{}, errMicrovmNotBuilt
}

func (*microvmRunnerStub) Release(context.Context) error { return nil }

// TestMicrovmStub_AcquireRefuses asserts Acquire returns the "not built" error
// meaning, and no Runner, when kvm presence is irrelevant (no predicate set).
func TestMicrovmStub_AcquireRefuses(t *testing.T) {
	m := &microvmStub{}

	runner, err := m.Acquire(context.Background(), loopauth.Principal{Tenant: "t-1", Subject: "s-1"}, Env{})
	if err == nil {
		t.Fatal("expected a refusal error, got nil")
	}
	if !errors.Is(err, errMicrovmNotBuilt) {
		t.Fatalf("expected errMicrovmNotBuilt, got %v", err)
	}
	if runner != nil {
		t.Fatalf("expected a nil Runner on refusal, got %#v", runner)
	}
}

// TestMicrovmStub_KVMAbsentFailsClosed is the load-bearing case: with kvm
// reported absent, Acquire MUST return an error and MUST NOT return anything
// satisfying the host handler's identity. Per ADR-0044's 2026-08-16 Update, a
// kvm-absent host fails CLOSED — it never degrades to the host/no-container
// off-switch, which would be fail-open.
func TestMicrovmStub_KVMAbsentFailsClosed(t *testing.T) {
	m := &microvmStub{kvmPresent: func() bool { return false }}

	runner, err := m.Acquire(context.Background(), loopauth.Principal{Tenant: "t-1", Subject: "s-1"}, Env{})

	if err == nil {
		t.Fatal("kvm-absent Acquire must return a non-nil error")
	}
	if runner != nil {
		t.Fatalf("kvm-absent Acquire must return a nil Runner, got %#v", runner)
	}

	// The stronger assertion: the returned value can never be mistaken for, or
	// satisfy the identity of, the host handler/runner. hostHandler's Runner is
	// *hostRunner; runner here is a bare nil Runner interface, which cannot be
	// type-asserted to *hostRunner at all.
	if _, ok := runner.(*hostRunner); ok {
		t.Fatal("kvm-absent Acquire must never return a host Runner")
	}
	var h Handler = &hostHandler{}
	if h.Name() == "" {
		t.Fatal("sanity: host handler identity must be a non-empty bounded name to compare against")
	}
	if m.Name() == h.Name() {
		t.Fatal("microvm stub identity must never collide with the host handler identity")
	}
}

// TestMicrovmStub_KVMPresentStillRefuses documents that presence of kvm alone
// does not make the stub usable — no handler is built, so it still refuses.
// This guards against a future accidental "kvm present => succeeds" edit
// smuggling in a real implementation through this conformance file.
func TestMicrovmStub_KVMPresentStillRefuses(t *testing.T) {
	m := &microvmStub{kvmPresent: func() bool { return true }}

	runner, err := m.Acquire(context.Background(), loopauth.Principal{Tenant: "t-1", Subject: "s-1"}, Env{})
	if err == nil {
		t.Fatal("expected a refusal error even with kvm present, got nil")
	}
	if runner != nil {
		t.Fatalf("expected a nil Runner, got %#v", runner)
	}
}

//go:build pgstore

// Package pgstore implements the Postgres-backed durable event store + loop
// registry for change 0047 — the DEPLOYABLE backend and the home of the shared,
// cross-instance LISTEN/NOTIFY pub/sub tail. It lives behind `//go:build pgstore`
// so the default `fuse` binary never imports pgx: bare `go test ./...` and the
// untagged build stay Postgres-free (the non-negotiable no-Postgres-import
// constraint).
//
// A single *PGStore serves every (tenant, loop) stream keyed by event.StreamKey
// against one database, so a loop's history and existence survive a restart and
// are reachable from any instance. It satisfies BOTH event.DurableStore and
// event.LoopRegistry (like fsstore's FSDurableStore), so a caller returns the one
// value for both seams.
//
// Invariants preserved:
//   - ADR-0024: payload is plaintext JSON in a jsonb column, independent of any
//     segment store.
//   - ADR-0025: seq is a PER-LOOP total order (PK (tenant_id, loop_id, seq)),
//     allocated by the store under a per-loop pg_advisory_xact_lock as
//     COALESCE(MAX(seq),0)+1 — NOT a global sequence. Callers pass Seq == 0.
//     Replay(from) is a clean seq > from cursor scoped to (tenant, loop).
//   - ADR-0016/ADR-0025: Append is a synchronous durable INSERT, then hands the
//     NOTIFY to a DECOUPLED bounded async publisher goroutine (drop-with-gap on
//     overflow) so Append NEVER blocks on NOTIFY delivery or a slow subscriber.
//   - Subscribe feeds a non-blocking drop-newest-with-gap channel from a dedicated
//     LISTEN connection; unsubscribe is idempotent.
package pgstore

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ethanhinson/fuse/internal/event"
)

//go:embed schema.sql
var schemaSQL string

// notifyChannel is the single Postgres LISTEN/NOTIFY channel every stream shares.
// The payload carries the (tenant, loop, seq) triple; the LISTEN worker fetches the
// row by that key, so a large event body never hits the 8000-byte NOTIFY payload
// limit and the 63-byte channel-identifier limit is never a factor.
const notifyChannel = "fuse_events"

// publishQueueDepth bounds the decoupled async publisher's backlog. Past it, a
// pending NOTIFY is dropped and the store records a gap — Append never blocks on
// NOTIFY delivery (ADR-0016/ADR-0025).
const publishQueueDepth = 1024

// subBuffer is the per-subscriber channel capacity. Past it, the LISTEN worker
// drops the newest event for that subscriber and records a gap, never blocking.
const subBuffer = 256

// PGStore is the Postgres durable event store + loop registry (change 0047). One
// instance serves many (tenant, loop) streams against a single pgxpool. It owns a
// decoupled async publisher goroutine (Append → bounded queue → NOTIFY) and a
// dedicated LISTEN connection that fans out to subscribers non-blockingly.
type PGStore struct {
	pool *pgxpool.Pool

	// pub is the decoupled async publisher: Append enqueues here (non-blocking),
	// the publisher goroutine drains it and issues NOTIFY. A full queue drops the
	// incoming NOTIFY (drop-newest-with-gap) and bumps dropped — Append never blocks
	// on NOTIFY.
	pub chan notifyMsg

	mu       sync.Mutex
	subs     map[int]*subscriber
	nextSub  int
	dropped  uint64
	closed   bool
	closeCh  chan struct{}
	listenWG sync.WaitGroup
}

// notifyMsg is one pending NOTIFY: the (tenant, loop, seq) triple the LISTEN side
// uses to fetch and deliver the row.
type notifyMsg struct {
	Tenant string    `json:"t"`
	Loop   string    `json:"l"`
	Seq    event.Seq `json:"s"`
}

type subscriber struct {
	key event.StreamKey
	ch  chan event.Event
}

// compile-time assertions.
var (
	_ event.DurableStore = (*PGStore)(nil)
	_ event.LoopRegistry = (*PGStore)(nil)
)

// Open connects to Postgres via a pgxpool, applies the embedded schema
// idempotently, and starts the decoupled async publisher + the dedicated LISTEN
// worker. The returned *PGStore satisfies event.DurableStore and event.LoopRegistry.
func Open(ctx context.Context, dsn string) (*PGStore, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("pgstore: pool: %w", err)
	}
	if _, err := pool.Exec(ctx, schemaSQL); err != nil {
		pool.Close()
		return nil, fmt.Errorf("pgstore: apply schema: %w", err)
	}
	s := &PGStore{
		pool:    pool,
		pub:     make(chan notifyMsg, publishQueueDepth),
		subs:    map[int]*subscriber{},
		closeCh: make(chan struct{}),
	}
	s.listenWG.Add(2)
	go s.publisherLoop()
	go s.listenLoop()
	return s, nil
}

// normalizeKey applies DefaultTenant so the empty tenant and DefaultTenant address
// the same stream.
func normalizeKey(k event.StreamKey) event.StreamKey {
	return event.StreamKey{Tenant: event.NormalizeTenant(k.Tenant), Loop: k.Loop}
}

// Append records one event on the (tenant, loop) stream. It allocates e.Seq
// per-loop from 1 under a transaction-scoped per-loop advisory lock (a PER-LOOP
// total order, NOT a global sequence — ADR-0025), writes durably, commits, then
// enqueues a NOTIFY on the decoupled async publisher (non-blocking: a full queue
// drops the notify and records a gap). Append NEVER blocks on NOTIFY delivery or a
// slow subscriber (ADR-0016).
func (s *PGStore) Append(ctx context.Context, key event.StreamKey, e event.Event) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	nk := normalizeKey(key)
	tenant := string(nk.Tenant)
	loop := string(nk.Loop)

	if e.TS.IsZero() {
		e.TS = time.Now().UTC()
	}
	var payload []byte
	if len(e.Payload) > 0 {
		payload = []byte(e.Payload)
	}

	var seq int64
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("pgstore: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Per-loop advisory lock: serialize Seq allocation for THIS (tenant, loop)
	// only, so concurrent Appends to different loops never contend and Seq stays a
	// per-loop total order. The lock is transaction-scoped (released on commit).
	if _, err := tx.Exec(ctx,
		`SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`,
		tenant+"/"+loop,
	); err != nil {
		return fmt.Errorf("pgstore: advisory lock: %w", err)
	}

	if err := tx.QueryRow(ctx,
		`INSERT INTO events (tenant_id, loop_id, seq, ts, kind, node_id, parent_id, depth, turn, payload)
		 VALUES ($1, $2,
		         (SELECT COALESCE(MAX(seq),0)+1 FROM events WHERE tenant_id=$1 AND loop_id=$2),
		         $3, $4, $5, $6, $7, $8, $9)
		 RETURNING seq`,
		tenant, loop, e.TS, string(e.Kind), e.NodeID, e.ParentID, e.Depth, e.Turn, payload,
	).Scan(&seq); err != nil {
		return fmt.Errorf("pgstore: insert: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("pgstore: commit: %w", err)
	}

	// Hand the NOTIFY to the decoupled async publisher. Non-blocking: a full queue
	// drops the notify and records a gap so Append never blocks on delivery.
	msg := notifyMsg{Tenant: tenant, Loop: loop, Seq: event.Seq(seq)}
	select {
	case s.pub <- msg:
	default:
		s.mu.Lock()
		s.dropped++
		s.mu.Unlock()
	}
	return nil
}

// Replay returns durable history for the (tenant, loop) stream with Seq > from
// (from == 0 ⇒ all), in Seq order, tenant-scoped (ADR-0025 cursor).
func (s *PGStore) Replay(ctx context.Context, key event.StreamKey, from event.Seq) ([]event.Event, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	nk := normalizeKey(key)
	rows, err := s.pool.Query(ctx,
		`SELECT seq, ts, kind, node_id, parent_id, depth, turn, payload
		 FROM events
		 WHERE tenant_id=$1 AND loop_id=$2 AND seq>$3
		 ORDER BY seq`,
		string(nk.Tenant), string(nk.Loop), int64(from),
	)
	if err != nil {
		return nil, fmt.Errorf("pgstore: replay query: %w", err)
	}
	defer rows.Close()

	var out []event.Event
	for rows.Next() {
		ev, err := scanEvent(rows)
		if err != nil {
			return out, err
		}
		out = append(out, ev)
	}
	if err := rows.Err(); err != nil {
		return out, err
	}
	return out, nil
}

// fetchOne reads a single event row by (tenant, loop, seq) for the LISTEN worker.
func (s *PGStore) fetchOne(ctx context.Context, tenant, loop string, seq event.Seq) (event.Event, error) {
	row := s.pool.QueryRow(ctx,
		`SELECT seq, ts, kind, node_id, parent_id, depth, turn, payload
		 FROM events WHERE tenant_id=$1 AND loop_id=$2 AND seq=$3`,
		tenant, loop, int64(seq),
	)
	return scanEvent(row)
}

// rowScanner is satisfied by both pgx.Row and pgx.Rows.
type rowScanner interface {
	Scan(dest ...any) error
}

func scanEvent(r rowScanner) (event.Event, error) {
	var (
		seq      int64
		ts       time.Time
		kind     string
		nodeID   string
		parentID string
		depth    int
		turn     int
		payload  []byte
	)
	if err := r.Scan(&seq, &ts, &kind, &nodeID, &parentID, &depth, &turn, &payload); err != nil {
		return event.Event{}, err
	}
	ev := event.Event{
		Seq:      event.Seq(seq),
		TS:       ts.UTC(),
		NodeID:   nodeID,
		ParentID: parentID,
		Depth:    depth,
		Turn:     turn,
		Kind:     event.Kind(kind),
	}
	if len(payload) > 0 {
		ev.Payload = json.RawMessage(payload)
	}
	return ev, nil
}

// Subscribe returns a live tail of the (tenant, loop) stream plus an idempotent
// unsubscribe closure. Delivery is non-blocking: the LISTEN worker drops the newest
// event and records a gap on a full buffer rather than back-pressuring Append.
func (s *PGStore) Subscribe(ctx context.Context, key event.StreamKey) (<-chan event.Event, func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, nil, fmt.Errorf("pgstore: closed")
	}
	id := s.nextSub
	s.nextSub++
	sub := &subscriber{key: normalizeKey(key), ch: make(chan event.Event, subBuffer)}
	s.subs[id] = sub

	var once sync.Once
	cancel := func() {
		once.Do(func() {
			s.mu.Lock()
			defer s.mu.Unlock()
			if cur, ok := s.subs[id]; ok {
				delete(s.subs, id)
				close(cur.ch)
			}
		})
	}
	return sub.ch, cancel, nil
}

// publisherLoop is the decoupled async publisher: it drains the bounded pub queue
// and issues one NOTIFY per pending message. It runs on its own connection so a
// slow NOTIFY never touches Append's path.
func (s *PGStore) publisherLoop() {
	defer s.listenWG.Done()
	for {
		select {
		case <-s.closeCh:
			return
		case msg := <-s.pub:
			payload, err := json.Marshal(msg)
			if err != nil {
				continue
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			_, _ = s.pool.Exec(ctx, `SELECT pg_notify($1, $2)`, notifyChannel, string(payload))
			cancel()
		}
	}
}

// listenLoop holds a dedicated connection LISTENing on notifyChannel. On each
// NOTIFY it fetches the row and fans it out to every subscriber whose StreamKey
// matches, with a strictly non-blocking send (drop-newest-with-gap). It reconnects
// on connection loss until Close.
func (s *PGStore) listenLoop() {
	defer s.listenWG.Done()
	for {
		select {
		case <-s.closeCh:
			return
		default:
		}
		s.listenOnce()
		// Brief backoff before reconnecting, unless closing.
		select {
		case <-s.closeCh:
			return
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// listenOnce acquires a dedicated connection, LISTENs, and blocks delivering
// notifications until the connection drops or the store closes.
func (s *PGStore) listenOnce() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Cancel the wait when Close fires.
	go func() {
		select {
		case <-s.closeCh:
			cancel()
		case <-ctx.Done():
		}
	}()

	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return
	}
	defer conn.Release()

	if _, err := conn.Exec(ctx, "LISTEN "+notifyChannel); err != nil {
		return
	}
	for {
		n, err := conn.Conn().WaitForNotification(ctx)
		if err != nil {
			return // connection lost or ctx canceled; listenLoop reconnects/exits
		}
		var msg notifyMsg
		if err := json.Unmarshal([]byte(n.Payload), &msg); err != nil {
			continue
		}
		s.deliver(ctx, msg)
	}
}

// deliver fetches the event named by msg and fans it out non-blockingly to every
// matching subscriber (drop-newest-with-gap).
func (s *PGStore) deliver(ctx context.Context, msg notifyMsg) {
	// Snapshot the ids of matching subscribers under the lock, then fetch the row
	// OUTSIDE the lock (a DB round-trip must not hold the mutex). We snapshot ids —
	// not the *subscriber values — so the send below can re-validate membership
	// under the re-acquired lock: a concurrent unsubscribe/Close closes sub.ch and
	// removes it from s.subs, so sending only to ids still present is what keeps this
	// from ever sending on a closed channel.
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	want := event.StreamKey{Tenant: event.TenantID(msg.Tenant), Loop: event.LoopID(msg.Loop)}
	var ids []int
	for id, sub := range s.subs {
		if sub.key == want {
			ids = append(ids, id)
		}
	}
	s.mu.Unlock()
	if len(ids) == 0 {
		return
	}

	fctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	ev, err := s.fetchOne(fctx, msg.Tenant, msg.Loop, msg.Seq)
	cancel()
	if err != nil {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	for _, id := range ids {
		// Re-validate membership under the held lock: unsubscribe/Close delete the
		// entry (and close its channel) under s.mu, so a still-present entry is
		// guaranteed open. Non-blocking send: a full buffer drops + records a gap.
		sub, ok := s.subs[id]
		if !ok {
			continue
		}
		select {
		case sub.ch <- ev:
		default:
			s.dropped++
		}
	}
}

// Register durably records a loop's existence (idempotent upsert keyed by the
// (tenant, loop) PK). CreatedAt is set on first registration; UpdatedAt advances.
func (s *PGStore) Register(ctx context.Context, rec event.LoopRecord) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	nk := normalizeKey(rec.Key)
	now := time.Now().UTC()
	_, err := s.pool.Exec(ctx,
		`INSERT INTO loops (tenant_id, loop_id, owner_node_id, live, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, $5, $5)
		 ON CONFLICT (tenant_id, loop_id) DO UPDATE
		   SET owner_node_id = EXCLUDED.owner_node_id,
		       live = EXCLUDED.live,
		       updated_at = EXCLUDED.updated_at`,
		string(nk.Tenant), string(nk.Loop), rec.OwnerNodeID, rec.Live, now,
	)
	if err != nil {
		return fmt.Errorf("pgstore: register: %w", err)
	}
	return nil
}

// SetLive flips the loop's liveness and records the owning node, preserving
// created_at. It returns ErrLoopUnknown if the loop was never registered.
func (s *PGStore) SetLive(ctx context.Context, key event.StreamKey, live bool, ownerNodeID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	nk := normalizeKey(key)
	tag, err := s.pool.Exec(ctx,
		`UPDATE loops SET live=$3, owner_node_id=$4, updated_at=$5
		 WHERE tenant_id=$1 AND loop_id=$2`,
		string(nk.Tenant), string(nk.Loop), live, ownerNodeID, time.Now().UTC(),
	)
	if err != nil {
		return fmt.Errorf("pgstore: setlive: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return event.ErrLoopUnknown
	}
	return nil
}

// Heartbeat renews the owner-liveness lease. It advances lease_expiry + updated_at
// (leaving owner/live untouched) and returns ErrLoopUnknown on 0 rows.
//
// TODO(#49 Task 3): the owner/lease_expiry columns and their round-trip through
// Register/Resolve/List are added in Task 3 (schema.sql + column wiring). This
// method currently renews only updated_at so the pgstore build stays green under
// the shared LoopRegistry interface; wire lease_expiry once the column exists.
func (s *PGStore) Heartbeat(ctx context.Context, key event.StreamKey, ownerNodeID string, expiry time.Time) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	nk := normalizeKey(key)
	tag, err := s.pool.Exec(ctx,
		`UPDATE loops SET owner_node_id=$3, updated_at=$4
		 WHERE tenant_id=$1 AND loop_id=$2`,
		string(nk.Tenant), string(nk.Loop), ownerNodeID, time.Now().UTC(),
	)
	if err != nil {
		return fmt.Errorf("pgstore: heartbeat: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return event.ErrLoopUnknown
	}
	return nil
}

// Resolve returns the loop's record, or ErrLoopUnknown if no row exists for the
// (tenant, loop) — including a resolve under the wrong tenant (tenant isolation).
func (s *PGStore) Resolve(ctx context.Context, key event.StreamKey) (event.LoopRecord, error) {
	if err := ctx.Err(); err != nil {
		return event.LoopRecord{}, err
	}
	nk := normalizeKey(key)
	var (
		owner     string
		live      bool
		createdAt time.Time
		updatedAt time.Time
	)
	err := s.pool.QueryRow(ctx,
		`SELECT owner_node_id, live, created_at, updated_at
		 FROM loops WHERE tenant_id=$1 AND loop_id=$2`,
		string(nk.Tenant), string(nk.Loop),
	).Scan(&owner, &live, &createdAt, &updatedAt)
	if err != nil {
		if err == pgx.ErrNoRows {
			return event.LoopRecord{}, event.ErrLoopUnknown
		}
		return event.LoopRecord{}, fmt.Errorf("pgstore: resolve: %w", err)
	}
	return event.LoopRecord{
		Key:         nk,
		OwnerNodeID: owner,
		Live:        live,
		CreatedAt:   createdAt.UTC(),
		UpdatedAt:   updatedAt.UTC(),
	}, nil
}

// List returns all loop records for a tenant (tenant-scoped; never another
// tenant's loops).
func (s *PGStore) List(ctx context.Context, tenant event.TenantID) ([]event.LoopRecord, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	nt := event.NormalizeTenant(tenant)
	rows, err := s.pool.Query(ctx,
		`SELECT loop_id, owner_node_id, live, created_at, updated_at
		 FROM loops WHERE tenant_id=$1 ORDER BY loop_id`,
		string(nt),
	)
	if err != nil {
		return nil, fmt.Errorf("pgstore: list: %w", err)
	}
	defer rows.Close()

	var out []event.LoopRecord
	for rows.Next() {
		var (
			loop      string
			owner     string
			live      bool
			createdAt time.Time
			updatedAt time.Time
		)
		if err := rows.Scan(&loop, &owner, &live, &createdAt, &updatedAt); err != nil {
			return out, err
		}
		out = append(out, event.LoopRecord{
			Key:         event.StreamKey{Tenant: nt, Loop: event.LoopID(loop)},
			OwnerNodeID: owner,
			Live:        live,
			CreatedAt:   createdAt.UTC(),
			UpdatedAt:   updatedAt.UTC(),
		})
	}
	if err := rows.Err(); err != nil {
		return out, err
	}
	return out, nil
}

// Dropped returns the total non-blocking-send / notify-queue drops so far, so the
// shared conformance suite's dropCounter check can assert a recorded gap without
// knowing the concrete type.
func (s *PGStore) Dropped() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.dropped
}

// Close stops the async publisher + LISTEN worker, closes all subscriber channels,
// and closes the pool. Idempotent.
func (s *PGStore) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	close(s.closeCh)
	for id, sub := range s.subs {
		delete(s.subs, id)
		close(sub.ch)
	}
	s.mu.Unlock()

	s.listenWG.Wait()
	s.pool.Close()
	return nil
}

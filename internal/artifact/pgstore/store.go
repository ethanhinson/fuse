//go:build pgstore

// Package pgstore is the Postgres artifact.Store backend. It lives behind the
// pgstore build tag so the default binary never imports pgx.
//
// One table holds every artifact, keyed by the full (tenant, loop, kind, id)
// ref. Bodies are gzip-compressed in memory before the INSERT; Open gunzips.
// Every read is by the full key, so a ref under another tenant is simply absent.
package pgstore

import (
	"bytes"
	"compress/gzip"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ethanhinson/fuse/internal/artifact"
	"github.com/ethanhinson/fuse/internal/event"
)

//go:embed schema.sql
var schemaSQL string

// schemaLockKey serializes schema application across replicas. It is distinct
// from the event store's key so the two schemas never wait on each other.
const schemaLockKey int64 = 0x66757365_61727466 // "fuseartf"

// Store is a Postgres artifact store. Several Store values (or processes) may
// share one database.
type Store struct {
	pool *pgxpool.Pool
}

var _ artifact.Store = (*Store)(nil)

// Open connects to Postgres with its own pool and applies the schema.
func Open(ctx context.Context, dsn string) (*Store, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("artifact pgstore: pool: %w", err)
	}
	if err := applySchema(ctx, pool); err != nil {
		pool.Close()
		return nil, fmt.Errorf("artifact pgstore: apply schema: %w", err)
	}
	return &Store{pool: pool}, nil
}

// Close releases the pool.
func (s *Store) Close() { s.pool.Close() }

// applySchema runs the DDL inside a transaction holding an advisory lock, so
// replicas starting together never race CREATE TABLE IF NOT EXISTS.
func applySchema(ctx context.Context, pool *pgxpool.Pool) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, schemaLockKey); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, schemaSQL); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// Put validates ref before any I/O, reads the body under the size cap, gzips it
// and upserts it. A non-empty ID replaces any existing artifact under that ref.
func (s *Store) Put(ctx context.Context, ref artifact.Ref, r io.Reader, meta artifact.Meta) (artifact.Meta, error) {
	ref.Tenant = event.NormalizeTenant(ref.Tenant)
	if err := ref.ValidateForPut(); err != nil {
		return artifact.Meta{}, err
	}
	if err := ctx.Err(); err != nil {
		return artifact.Meta{}, err
	}
	body, err := artifact.ReadCapped(r)
	if err != nil {
		return artifact.Meta{}, err
	}
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	if _, err := zw.Write(body); err != nil {
		return artifact.Meta{}, err
	}
	if err := zw.Close(); err != nil {
		return artifact.Meta{}, err
	}
	if ref.ID == "" {
		ref.ID = artifact.NewID()
	}
	meta.Ref = ref
	meta.Size = int64(len(body))
	// timestamptz keeps microseconds; truncate so the column and the meta JSON
	// agree and List orders by exactly what Open reports.
	meta.CreatedAt = time.Now().UTC().Truncate(time.Microsecond)
	meta.Labels = maps.Clone(meta.Labels)
	mb, err := json.Marshal(meta)
	if err != nil {
		return artifact.Meta{}, err
	}
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO artifacts (tenant_id, loop_id, kind, id, meta, body, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (tenant_id, loop_id, kind, id) DO UPDATE
		SET meta = EXCLUDED.meta, body = EXCLUDED.body, created_at = EXCLUDED.created_at`,
		string(ref.Tenant), string(ref.Loop), string(ref.Kind), ref.ID, mb, gz.Bytes(), meta.CreatedAt); err != nil {
		return artifact.Meta{}, fmt.Errorf("artifact pgstore: put %s: %w", ref, err)
	}
	out := meta
	out.Labels = maps.Clone(meta.Labels)
	return out, nil
}

// Open selects by the full key and returns the gunzipped body. A missing ref,
// or one under another tenant, is ErrNotFound.
func (s *Store) Open(ctx context.Context, ref artifact.Ref) (io.ReadCloser, artifact.Meta, error) {
	ref.Tenant = event.NormalizeTenant(ref.Tenant)
	if err := ref.Validate(); err != nil {
		return nil, artifact.Meta{}, err
	}
	var mb, gz []byte
	err := s.pool.QueryRow(ctx, `
		SELECT meta, body FROM artifacts
		WHERE tenant_id = $1 AND loop_id = $2 AND kind = $3 AND id = $4`,
		string(ref.Tenant), string(ref.Loop), string(ref.Kind), ref.ID).Scan(&mb, &gz)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, artifact.Meta{}, artifact.ErrNotFound
	}
	if err != nil {
		return nil, artifact.Meta{}, fmt.Errorf("artifact pgstore: open %s: %w", ref, err)
	}
	meta, err := decodeMeta(mb)
	if err != nil {
		return nil, artifact.Meta{}, err
	}
	if meta.Ref != ref {
		// A row whose meta names another ref is never served.
		return nil, artifact.Meta{}, artifact.ErrNotFound
	}
	zr, err := gzip.NewReader(bytes.NewReader(gz))
	if err != nil {
		return nil, artifact.Meta{}, fmt.Errorf("artifact pgstore: %s: corrupt body: %w", ref, err)
	}
	body, err := io.ReadAll(zr)
	if err != nil {
		return nil, artifact.Meta{}, fmt.Errorf("artifact pgstore: %s: corrupt body: %w", ref, err)
	}
	return io.NopCloser(bytes.NewReader(body)), meta, nil
}

// List returns the (tenant, loop, kind) artifacts ordered by CreatedAt, then ID.
func (s *Store) List(ctx context.Context, tenant event.TenantID, loop event.LoopID, kind artifact.Kind) ([]artifact.Meta, error) {
	tenant = event.NormalizeTenant(tenant)
	if err := artifact.ValidateScope(tenant, loop, kind); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id, meta FROM artifacts
		WHERE tenant_id = $1 AND loop_id = $2 AND kind = $3
		ORDER BY created_at, id`,
		string(tenant), string(loop), string(kind))
	if err != nil {
		return nil, fmt.Errorf("artifact pgstore: list: %w", err)
	}
	defer rows.Close()
	var out []artifact.Meta
	for rows.Next() {
		var id string
		var mb []byte
		if err := rows.Scan(&id, &mb); err != nil {
			return nil, err
		}
		m, err := decodeMeta(mb)
		if err != nil {
			return nil, err
		}
		if m.Ref != (artifact.Ref{Tenant: tenant, Loop: loop, Kind: kind, ID: id}) {
			continue // a meta naming another ref is never served
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("artifact pgstore: list: %w", err)
	}
	// The SQL already orders; sorting the decoded metas keeps the order
	// identical to every other backend's.
	artifact.SortMetas(out)
	return out, nil
}

func decodeMeta(b []byte) (artifact.Meta, error) {
	var m artifact.Meta
	if err := json.Unmarshal(b, &m); err != nil {
		return artifact.Meta{}, fmt.Errorf("artifact pgstore: corrupt meta: %w", err)
	}
	return m, nil
}

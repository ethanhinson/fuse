//go:build pgstore

// Package pgsource implements the Postgres-backed skill Source for change
// 0134 — the CROSS-INSTANCE installed-skill home. A single skills table is
// shared by every replica, so each replica sources the SAME installed skill
// set: replicas advertise identical capabilities and a resume on any replica
// resolves the same skill references. It lives behind `//go:build pgstore`
// (like internal/event/pgstore and internal/artifact/pgstore) so the default
// `fuse` binary never imports pgx.
//
// The table is `skills(name text PRIMARY KEY, description text, body text,
// updated_at timestamptz)`, idempotently applied on Open. A skill's name is
// its key; writing a row upserts it, deleting a row removes it. Load returns
// the whole table as a skills.Set in name order and its digest.
package pgsource

import (
	"context"
	_ "embed"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ethanhinson/fuse/internal/skills"
)

//go:embed schema.sql
var schemaSQL string

// schemaLockKey serializes schema application across replicas. It is distinct
// from the event store's and artifact store's keys so the three never wait on
// each other.
const schemaLockKey int64 = 0x66757365_736B6C73 // "fuseskls"

// Store is the Postgres skill source: one shared pool, the whole skills table
// as the installed set.
type Store struct {
	pool *pgxpool.Pool
}

var _ skills.Source = (*Store)(nil)

// Open connects to Postgres with its own pool and applies the schema.
func Open(ctx context.Context, dsn string) (*Store, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("skills pgsource: pool: %w", err)
	}
	if err := applySchema(ctx, pool); err != nil {
		pool.Close()
		return nil, fmt.Errorf("skills pgsource: apply schema: %w", err)
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

// Load returns the whole skills table as a skills.Set, sorted by name, and
// the set's digest. It is safe to call repeatedly (a refresh ticker calls it
// each tick to detect changes).
func (s *Store) Load(ctx context.Context) (*skills.Set, string, error) {
	rows, err := s.pool.Query(ctx, `SELECT name, description, body FROM skills ORDER BY name`)
	if err != nil {
		return nil, "", fmt.Errorf("skills pgsource: query: %w", err)
	}
	defer rows.Close()

	var list []skills.Skill
	for rows.Next() {
		var name, description, body string
		if err := rows.Scan(&name, &description, &body); err != nil {
			return nil, "", fmt.Errorf("skills pgsource: scan: %w", err)
		}
		sk := skills.NewCarried(name, description, body)
		// Carry path is "spec" by default; stamp it so a Postgres skill is
		// distinguishable from a carried one in the set.
		sk.Path = "pg"
		list = append(list, sk)
	}
	if err := rows.Err(); err != nil {
		return nil, "", fmt.Errorf("skills pgsource: rows: %w", err)
	}
	out := skills.NewSet(list)
	return out, skills.DigestOf(out), nil
}

// Upsert installs or updates one skill (used by tests and an operator tool;
// the server itself only reads).
func (s *Store) Upsert(ctx context.Context, name, description, body string) error {
	if _, err := s.pool.Exec(ctx,
		`INSERT INTO skills (name, description, body, updated_at)
		 VALUES ($1, $2, $3, now())
		 ON CONFLICT (name) DO UPDATE SET description = EXCLUDED.description, body = EXCLUDED.body, updated_at = now()`,
		name, description, body); err != nil {
		return fmt.Errorf("skills pgsource: upsert: %w", err)
	}
	return nil
}

// Delete removes one skill (used by tests).
func (s *Store) Delete(ctx context.Context, name string) error {
	if _, err := s.pool.Exec(ctx, `DELETE FROM skills WHERE name = $1`, name); err != nil {
		return fmt.Errorf("skills pgsource: delete: %w", err)
	}
	return nil
}
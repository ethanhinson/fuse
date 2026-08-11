-- schema.sql is the embedded, idempotent DDL for the Postgres durable event store
-- (change 0047). Open applies it on connect with CREATE ... IF NOT EXISTS so a
-- fresh database and an existing one both converge to this shape without a
-- migration tool.
--
-- ADR-0024: payload is plaintext JSON (jsonb), independent of any segment store.
-- ADR-0025: seq is a PER-LOOP total order — the PK is (tenant_id, loop_id, seq),
--           NOT a global sequence. Append allocates it under a per-loop advisory
--           lock as COALESCE(MAX(seq),0)+1 scoped to (tenant_id, loop_id).

CREATE TABLE IF NOT EXISTS events (
    tenant_id text        NOT NULL,
    loop_id   text        NOT NULL,
    seq       bigint      NOT NULL,
    ts        timestamptz NOT NULL,
    kind      text        NOT NULL,
    node_id   text        NOT NULL DEFAULT '',
    parent_id text        NOT NULL DEFAULT '',
    depth     int         NOT NULL DEFAULT 0,
    turn      int         NOT NULL DEFAULT 0,
    payload   jsonb,
    PRIMARY KEY (tenant_id, loop_id, seq)
);

CREATE TABLE IF NOT EXISTS loops (
    tenant_id     text        NOT NULL,
    loop_id       text        NOT NULL,
    owner_node_id text        NOT NULL DEFAULT '',
    live          boolean     NOT NULL DEFAULT false,
    created_at    timestamptz NOT NULL,
    updated_at    timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, loop_id)
);

-- schema.sql is the embedded, idempotent DDL for the Postgres skill source
-- (change 0134). Open applies it on connect with CREATE TABLE IF NOT EXISTS so
-- a fresh database and an existing one both converge to this shape without a
-- migration tool.
--
-- A skill's name is its primary key: writing a row upserts it, deleting a row
-- removes it. updated_at lets operators / a future management surface order
-- writes; Load reads the whole table.

CREATE TABLE IF NOT EXISTS skills (
    name        text        PRIMARY KEY,
    description text        NOT NULL DEFAULT '',
    body        text        NOT NULL,
    updated_at  timestamptz NOT NULL DEFAULT now()
);
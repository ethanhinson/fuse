-- Artifact store schema. Applied idempotently by Open under an advisory lock.
-- body is the gzip-compressed artifact; meta is the artifact.Meta JSON.
CREATE TABLE IF NOT EXISTS artifacts (
    tenant_id  text        NOT NULL,
    loop_id    text        NOT NULL,
    kind       text        NOT NULL,
    id         text        NOT NULL,
    meta       jsonb       NOT NULL,
    body       bytea       NOT NULL,
    created_at timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, loop_id, kind, id)
);

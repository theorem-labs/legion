-- Ready migration records are the exact landing inputs an owner declared. A later ready replaces the record;
-- a takeover, extension, abandonment or failed landing round clears all ready columns together.
ALTER TABLE migrations
    ADD COLUMN ready_head text,
    ADD COLUMN ready_at timestamptz,
    ADD COLUMN ready_event bigint,
    ADD COLUMN ready_changed_contracts jsonb,
    ADD COLUMN ready_records jsonb;
CREATE INDEX migrations_ready_event ON migrations (repo, ready_event) WHERE ready_event IS NOT NULL;

-- L4 configures and validates this owner for upstream provenance.
ALTER TABLE repositories ADD COLUMN upstream_owner text;

-- L5 records idempotency keys for landing rounds.
CREATE TABLE round_keys (
    repo text NOT NULL,
    migration text NOT NULL,
    key text NOT NULL,
    PRIMARY KEY (repo, migration, key),
    FOREIGN KEY (repo, migration) REFERENCES migrations(repo, id)
);

-- L6 records once per owner revision why the supervisor declined to start it.
CREATE TABLE owner_refusals (
    repo text NOT NULL REFERENCES repositories(name),
    owner text NOT NULL,
    revision text NOT NULL,
    reason text NOT NULL,
    at timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (repo, owner, revision)
);

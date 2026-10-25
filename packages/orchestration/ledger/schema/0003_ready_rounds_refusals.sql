-- A ready migration's record is the exact statement records an owner declared for its landing. A later ready replaces
-- it; a takeover, extension, abandonment or failed landing round clears all ready columns together.
ALTER TABLE migrations
    ADD COLUMN ready_at timestamptz,
    ADD COLUMN ready_event bigint,
    ADD COLUMN ready_records jsonb;
CREATE INDEX migrations_ready_event ON migrations (repo, ready_event) WHERE ready_event IS NOT NULL;

-- The idempotency key of each ready submission and the migration.ready event it published, so a retried submission
-- returns what it stored and publishes nothing, even after a round consumed its record.
CREATE TABLE ready_keys (
    repo text NOT NULL,
    migration text NOT NULL,
    key text NOT NULL,
    ready_event bigint NOT NULL,
    PRIMARY KEY (repo, migration, key),
    FOREIGN KEY (repo, migration) REFERENCES migrations(repo, id)
);

-- The record that advanced each contract's statement: its compatibility proof or its owner's review, with the
-- upstream commit beside it for an upstream landing.
ALTER TABLE contracts ADD COLUMN last_change jsonb;

-- The idempotency key of each recorded landing round, so a retried round counts once: the first round with a key is
-- recorded, and a repeat of that key replays the migration as it is.
CREATE TABLE round_keys (
    repo text NOT NULL,
    migration text NOT NULL,
    key text NOT NULL,
    PRIMARY KEY (repo, migration, key),
    FOREIGN KEY (repo, migration) REFERENCES migrations(repo, id)
);

-- Why the supervisor declined to start an owner, recorded once per owner and work revision (an event id).
CREATE TABLE owner_refusals (
    repo text NOT NULL REFERENCES repositories(name),
    owner text NOT NULL,
    revision bigint NOT NULL,
    reason text NOT NULL,
    at timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (repo, owner, revision)
);

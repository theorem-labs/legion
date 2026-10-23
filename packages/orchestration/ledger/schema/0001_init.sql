-- The orchestration ledger: work-item leases, migrations with their change locks, attempt records and the event log.
-- All times come from the database clock, so every host agrees on when a lease ends.

CREATE TABLE repositories (
    name               text PRIMARY KEY,
    max_landing_rounds integer NOT NULL CHECK (max_landing_rounds >= 0),
    created_at         timestamptz NOT NULL DEFAULT now()
);

-- A unit of work with an expiring lease. generation rises on every grant (first claim, re-grant after expiry or
-- release); a renew, release or result must carry the current generation.
CREATE TABLE work_items (
    repo             text NOT NULL REFERENCES repositories(name),
    id               text NOT NULL,
    state            text NOT NULL DEFAULT 'open' CHECK (state IN ('open', 'done')),
    holder           text,
    generation       bigint NOT NULL DEFAULT 0,
    lease_granted_at timestamptz,
    lease_expires_at timestamptz,
    result           text,
    created_at       timestamptz NOT NULL DEFAULT now(),
    updated_at       timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (repo, id),
    CHECK ((holder IS NULL) = (lease_expires_at IS NULL))
);
CREATE INDEX work_items_held ON work_items (repo, lease_expires_at) WHERE holder IS NOT NULL;

-- A contract change and its callers on one branch. While open, its declared writes are its change lock.
CREATE TABLE migrations (
    repo              text NOT NULL REFERENCES repositories(name),
    id                text NOT NULL,
    owner             text NOT NULL,
    base_commit       text NOT NULL DEFAULT '',
    branch            text NOT NULL DEFAULT '',
    state             text NOT NULL DEFAULT 'open' CHECK (state IN ('open', 'landed', 'abandoned')),
    generation        bigint NOT NULL DEFAULT 1 CHECK (generation >= 1),
    landing_rounds    integer NOT NULL DEFAULT 0,
    escalated_at      timestamptz,
    escalation_reason text,
    opened_at         timestamptz NOT NULL DEFAULT now(),
    closed_at         timestamptz,
    close_reason      text,
    PRIMARY KEY (repo, id),
    CHECK ((state = 'open') = (closed_at IS NULL))
);

-- Declared writes. is_prefix marks a directory: it covers every path below it, new files included.
CREATE TABLE migration_writes (
    repo      text NOT NULL,
    migration text NOT NULL,
    path      text NOT NULL,
    is_prefix boolean NOT NULL,
    PRIMARY KEY (repo, migration, path, is_prefix),
    FOREIGN KEY (repo, migration) REFERENCES migrations(repo, id)
);

-- Declared reads: a declaration (or, for records imported from the pilot, a file path) and the statement hash
-- the migration relied on.
CREATE TABLE migration_reads (
    repo      text NOT NULL,
    migration text NOT NULL,
    name      text NOT NULL,
    hash      text NOT NULL,
    PRIMARY KEY (repo, migration, name),
    FOREIGN KEY (repo, migration) REFERENCES migrations(repo, id)
);

-- One attempt to run a logical owner (a role) on a host. The row is written, as intent, before the process starts.
CREATE TABLE attempts (
    id                bigserial PRIMARY KEY,
    repo              text NOT NULL REFERENCES repositories(name),
    logical_owner     text NOT NULL,
    host              text NOT NULL,
    generation        bigint NOT NULL,
    desired_state     text NOT NULL CHECK (desired_state IN ('running', 'stopped')),
    observed_state    text NOT NULL CHECK (observed_state IN ('pending', 'running', 'exited', 'lost')),
    checkpoint_branch text NOT NULL DEFAULT '',
    budget_usd        numeric(12, 2) NOT NULL DEFAULT 0 CHECK (budget_usd >= 0),
    created_at        timestamptz NOT NULL DEFAULT now(),
    started_at        timestamptz,
    ended_at          timestamptz,
    exit_reason       text CHECK (exit_reason IN ('completed', 'refused', 'transient_error', 'credential_expired',
                                                  'quota_exhausted', 'budget_exhausted', 'superseded', 'crashed',
                                                  'killed', 'lost')),
    exit_detail       text,
    UNIQUE (repo, logical_owner, generation),
    CHECK ((ended_at IS NULL) = (exit_reason IS NULL))
);

-- Every change, in order. Rows are never updated or deleted.
CREATE TABLE events (
    id       bigserial PRIMARY KEY,
    at       timestamptz NOT NULL DEFAULT clock_timestamp(),
    repo     text NOT NULL,
    kind     text NOT NULL,
    subject  text NOT NULL,
    actor    text NOT NULL DEFAULT '',
    data     jsonb NOT NULL DEFAULT '{}'
);
CREATE INDEX events_subject ON events (repo, subject, id);

CREATE FUNCTION events_append_only() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'the event log is append-only';
END $$;
CREATE TRIGGER events_append_only BEFORE UPDATE OR DELETE OR TRUNCATE ON events
    FOR EACH STATEMENT EXECUTE FUNCTION events_append_only();

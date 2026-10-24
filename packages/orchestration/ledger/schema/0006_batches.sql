-- Every event names the database login whose session appended it. The default is evaluated once for the rows that
-- exist when this schema is applied, so each of them names the login that applied it.
ALTER TABLE events ADD COLUMN login text NOT NULL DEFAULT session_user;

-- The landing program's durable batch. record is the program's whole batch record, stored as the program sends it;
-- phase is its state, computed by Postgres on every write. record_version orders every write: a write names the
-- version it read. admission is admitted while the batch's members are fenced, then landed or declined.
CREATE TABLE batches (
    repo             text NOT NULL REFERENCES repositories(name),
    id               text NOT NULL,
    record           jsonb NOT NULL,
    phase            text NOT NULL GENERATED ALWAYS AS (coalesce(record->>'state', '')) STORED,
    record_version   bigint NOT NULL CHECK (record_version >= 1),
    admission        text CHECK (admission IN ('admitted', 'landed', 'declined')),
    admission_digest text,
    decline_digest   text,
    successor        text,
    created_at       timestamptz NOT NULL,
    updated_at       timestamptz NOT NULL,
    PRIMARY KEY (repo, id)
);
CREATE INDEX batches_created ON batches (repo, created_at, id);

-- A batch's frozen members, in position order. A migration is admitted in at most one batch at a time; releasing the
-- admission (admitted = false) lets a declined migration join a new batch.
CREATE TABLE batch_members (
    repo        text NOT NULL,
    batch       text NOT NULL,
    migration   text NOT NULL,
    position    integer NOT NULL,
    generation  bigint NOT NULL,
    head        text NOT NULL,
    ready_event bigint NOT NULL,
    admitted    boolean NOT NULL DEFAULT false,
    PRIMARY KEY (repo, batch, migration),
    UNIQUE (repo, batch, position),
    FOREIGN KEY (repo, batch) REFERENCES batches(repo, id),
    FOREIGN KEY (repo, migration) REFERENCES migrations(repo, id)
);
CREATE UNIQUE INDEX batch_members_admitted ON batch_members (repo, migration) WHERE admitted;

-- A batch's post-landing effects: each an intent until it is done, never back. A key is scoped to its batch. member is
-- empty for an effect of the whole batch. result holds the intent's inputs (its landing commit) and then the done
-- result; last_attempt is the newest failed attempt's result, and held stops the effect until an operator acts.
CREATE TABLE batch_effects (
    repo              text NOT NULL,
    batch             text NOT NULL,
    key               text NOT NULL,
    member            text NOT NULL DEFAULT '',
    op                text NOT NULL,
    kind              text NOT NULL DEFAULT '',
    definition_digest text NOT NULL DEFAULT '',
    state             text NOT NULL DEFAULT 'intent' CHECK (state IN ('intent', 'done')),
    held              boolean NOT NULL DEFAULT false,
    attempts          integer NOT NULL DEFAULT 0,
    last_attempt      jsonb,
    result            jsonb NOT NULL DEFAULT '{}',
    at                timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (repo, batch, key),
    FOREIGN KEY (repo, batch) REFERENCES batches(repo, id)
);
CREATE INDEX batch_effects_intent ON batch_effects (repo, batch) WHERE state = 'intent';

-- The files a done effect produced, each with its SHA-256.
CREATE TABLE batch_effect_outputs (
    repo   text NOT NULL,
    batch  text NOT NULL,
    key    text NOT NULL,
    name   text NOT NULL,
    sha256 text NOT NULL,
    bytes  bytea NOT NULL,
    PRIMARY KEY (repo, batch, key, name),
    FOREIGN KEY (repo, batch, key) REFERENCES batch_effects(repo, batch, key)
);

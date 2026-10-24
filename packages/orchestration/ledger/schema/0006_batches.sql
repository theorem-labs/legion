-- Every event names the database login whose session appended it. The default is evaluated once for the rows that
-- exist when this schema is applied, so each of them names the login that applied it.
ALTER TABLE events ADD COLUMN login text NOT NULL DEFAULT session_user;

-- The owners a landing escalation may be enqueued for, as a sorted set: `repo ensure --escalation-owner`.
ALTER TABLE repositories ADD COLUMN escalation_owners text[] NOT NULL DEFAULT '{}';

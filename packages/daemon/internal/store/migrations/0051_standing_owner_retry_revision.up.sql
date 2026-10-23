-- The queue revision that exhausted a standing owner's retry allowance. A later queue revision,
-- or an operator retry, is the only way to reset that allowance.
ALTER TABLE claims ADD COLUMN standing_owner_retry_revision bigint NOT NULL DEFAULT 0 CHECK (standing_owner_retry_revision >= 0);

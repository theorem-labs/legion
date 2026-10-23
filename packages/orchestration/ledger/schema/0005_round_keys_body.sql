-- A round key is idempotent only for the original generation, reason and batch.
-- Existing rows cannot be reconstructed, so refuse this migration rather than assigning invented bodies.
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM round_keys) THEN
        RAISE EXCEPTION 'cannot add round key body: round_keys already contains rows';
    END IF;
END $$;

ALTER TABLE round_keys
    ADD COLUMN generation bigint NOT NULL,
    ADD COLUMN reason text NOT NULL,
    ADD COLUMN batch text NOT NULL;

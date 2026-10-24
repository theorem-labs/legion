-- The record that advanced each contract's statement: its compatibility proof, its owner's review, or the upstream
-- commit with the reviews.
ALTER TABLE contracts ADD COLUMN last_change jsonb;

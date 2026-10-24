-- The record that advanced each contract's statement: its compatibility proof or its owner's review, with the
-- upstream commit beside it for an upstream landing.
ALTER TABLE contracts ADD COLUMN last_change jsonb;

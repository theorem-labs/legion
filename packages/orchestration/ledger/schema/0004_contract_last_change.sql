-- L4 records the proof, review, or upstream provenance that advanced each contract.
ALTER TABLE contracts ADD COLUMN last_change jsonb;

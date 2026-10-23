-- A financial reservation survives every crash; capacity is separate and ends when a pre-launch
-- attempt is abandoned before it records a running claim.
ALTER TABLE standing_owner_attempts ADD COLUMN capacity_reserved boolean NOT NULL DEFAULT true;

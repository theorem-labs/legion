-- A standing owner's ledger key: the graph node id (`function:get_mountpoint`) or named role whose
-- queue it serves, and the holder name its leases are taken under. Its token encodes the key; the
-- claim records it so logs and the operator endpoint show the key itself.
ALTER TABLE claims ADD COLUMN owner_key text NOT NULL DEFAULT '';
ALTER TABLE claims ADD CONSTRAINT claims_owner_key_is_an_owners CHECK (owner_key = '' OR role = 'owner');

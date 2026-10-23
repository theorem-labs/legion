-- A queued item names the standing owner that may act on it. Existing unassigned items keep the
-- empty owner, which never wakes a configured owner.
ALTER TABLE work_items ADD COLUMN owner text NOT NULL DEFAULT '';
CREATE INDEX work_items_open_owner ON work_items (repo, owner) WHERE state = 'open';

-- A lease has a holder. A supervised lease (one a supervisor watches) has no end; the supervisor
-- frees it when it finds the holder stopped. Only an unsupervised lease carries a fixed end.
ALTER TABLE work_items DROP CONSTRAINT work_items_check;
ALTER TABLE work_items ADD CONSTRAINT work_items_lease_end_has_holder
    CHECK (lease_expires_at IS NULL OR holder IS NOT NULL);
CREATE INDEX work_items_holder ON work_items (repo, holder) WHERE holder IS NOT NULL;

-- A queued item names the standing owner that may act on it. Existing unassigned items keep the
-- empty owner, which never wakes a configured owner.
ALTER TABLE work_items ADD COLUMN owner text NOT NULL DEFAULT '';
CREATE INDEX work_items_open_owner ON work_items (repo, owner) WHERE state = 'open';

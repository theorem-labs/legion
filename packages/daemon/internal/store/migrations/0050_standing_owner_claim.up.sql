-- A standing owner has no Dispatch issue or tree and is supervised from a ledger queue.
ALTER TABLE claims DROP CONSTRAINT claims_role_check;
ALTER TABLE claims ADD CONSTRAINT claims_role_check CHECK (role IN (
  'architect', 'planner', 'implementer', 'tester', 'reviewer', 'merger', 'controller', 'owner'));
ALTER TABLE claims DROP CONSTRAINT claims_controller_shape;
ALTER TABLE claims ADD CONSTRAINT claims_tree_independent_shape CHECK (
  (role IN ('controller', 'owner')) = (issue = '' AND tree = ''));

CREATE TABLE standing_owner_attempts (
  owner_token text NOT NULL REFERENCES claims(token) ON DELETE CASCADE,
  generation bigint NOT NULL CHECK (generation > 0),
  reserved_usd double precision NOT NULL CHECK (reserved_usd > 0),
  actual_usd double precision CHECK (actual_usd >= 0),
  created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
  PRIMARY KEY (owner_token, generation)
);

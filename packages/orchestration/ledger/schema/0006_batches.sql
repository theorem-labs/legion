-- Every event names the database login whose session appended it. The default is evaluated once for the rows that
-- exist when this schema is applied, so each of them names the login that applied it.
ALTER TABLE events ADD COLUMN login text NOT NULL DEFAULT session_user;

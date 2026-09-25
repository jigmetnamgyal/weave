-- +goose Up
-- +goose StatementBegin

-- The branch a session's workflow actually cut (Unit M5.2).
--
-- 00007 recorded the branch *intent* — `branch_name` and `base_branch` — and
-- said plainly that it was not a record that the branch exists. This is that
-- record: the commit the branch pointed at when the workflow confirmed it.
-- M5.4's checkout needs the value anyway, and it is the durable answer to
-- "was this session's branch made?" that does not depend on an audit row.
--
-- On the session rather than on a transition row, because cutting the branch
-- is not a state change: it happens *during* `provisioning`, and inventing a
-- transition to carry it would record a move the session never made.
--
-- Nullable and additive, so the previous application version runs against it
-- unchanged: sqlc expands `SELECT *` into an explicit column list at
-- generation time, and an old binary simply never reads the column.
ALTER TABLE sessions ADD COLUMN branch_sha text;

-- A commit id, and nothing else. SHA-1 today, SHA-256 when GitHub offers it.
-- Lower-case hex because that is what the API returns; a value in any other
-- shape came from somewhere it should not have.
ALTER TABLE sessions ADD CONSTRAINT sessions_branch_sha_format
    CHECK (branch_sha IS NULL OR branch_sha ~ '^[0-9a-f]{40}([0-9a-f]{24})?$');

-- Written once. The application only ever sets it where it is NULL, and this
-- makes that true for every path rather than for the one that remembered:
-- a session whose recorded branch commit could be rewritten afterwards is one
-- whose checkout could be pointed at different work than the one it started
-- from, silently.
CREATE FUNCTION sessions_branch_sha_write_once() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF OLD.branch_sha IS NOT NULL AND NEW.branch_sha IS DISTINCT FROM OLD.branch_sha THEN
        RAISE EXCEPTION 'sessions.branch_sha is written once and cannot be changed'
            USING ERRCODE = 'check_violation',
                  CONSTRAINT = 'sessions_branch_sha_write_once';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER sessions_branch_sha_write_once
    BEFORE UPDATE OF branch_sha ON sessions
    FOR EACH ROW EXECUTE FUNCTION sessions_branch_sha_write_once();

COMMENT ON COLUMN sessions.branch_sha IS 'The commit the session branch pointed at when the workflow confirmed it. NULL until then; written once.';

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP TRIGGER IF EXISTS sessions_branch_sha_write_once ON sessions;
DROP FUNCTION IF EXISTS sessions_branch_sha_write_once();
ALTER TABLE sessions DROP CONSTRAINT IF EXISTS sessions_branch_sha_format;
ALTER TABLE sessions DROP COLUMN IF EXISTS branch_sha;

-- +goose StatementEnd

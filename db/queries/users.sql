-- name: GetUserByExternalID :one
SELECT * FROM users
WHERE external_id = $1;

-- name: UpsertUserByExternalID :one
-- Just-in-time provisioning. ON CONFLICT makes concurrent first requests for
-- the same subject resolve to a single row instead of racing, and DO UPDATE
-- (rather than DO NOTHING) guarantees RETURNING yields the row either way.
INSERT INTO users (id, external_id, email, display_name, avatar_url)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (external_id) DO UPDATE
SET email        = EXCLUDED.email,
    display_name = EXCLUDED.display_name,
    avatar_url   = EXCLUDED.avatar_url,
    updated_at   = now()
RETURNING *;

-- name: CountUsers :one
-- Test and operator support only; not used on a request path.
SELECT count(*) FROM users;

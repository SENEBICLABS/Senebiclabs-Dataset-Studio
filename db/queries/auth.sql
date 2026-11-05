-- Authentication store (M3). Every statement runs as studio_app and uses only
-- the columns that role is granted (plan 6.3).

-- name: GetLoginCandidate :one
-- Login lookup by email (case-insensitive via citext).
SELECT u.principal_id, u.password_hash, p.status
FROM studio.users u
JOIN studio.principals p ON p.id = u.principal_id
WHERE u.email = sqlc.arg(email)::text;

-- name: CreateSession :exec
INSERT INTO studio.sessions (id, principal_id, token_hash, created_at, expires_at, last_seen_at, ip, user_agent)
VALUES (sqlc.arg(id), sqlc.arg(principal_id), sqlc.arg(token_hash), sqlc.arg(created_at),
        sqlc.arg(expires_at), sqlc.arg(created_at), sqlc.narg(ip), sqlc.narg(user_agent));

-- name: GetSessionByTokenHash :one
SELECT s.id, s.principal_id, s.expires_at, s.last_seen_at, s.revoked_at, p.kind, p.status
FROM studio.sessions s
JOIN studio.principals p ON p.id = s.principal_id
WHERE s.token_hash = sqlc.arg(token_hash);

-- name: TouchSession :exec
-- Refreshes last_seen_at at most once a minute (plan 6.3); not audited.
UPDATE studio.sessions
SET last_seen_at = sqlc.arg(now)
WHERE id = sqlc.arg(id)
  AND revoked_at IS NULL
  AND last_seen_at < sqlc.arg(now)::timestamptz - interval '1 minute';

-- name: RevokeSession :execrows
UPDATE studio.sessions
SET revoked_at = sqlc.arg(now)
WHERE id = sqlc.arg(id) AND revoked_at IS NULL;

-- name: CreateAPIKey :exec
INSERT INTO studio.api_keys (id, principal_id, principal_kind, prefix, key_hash, created_at, created_by, expires_at)
VALUES (sqlc.arg(id), sqlc.arg(principal_id), sqlc.arg(principal_kind), sqlc.arg(prefix), sqlc.arg(key_hash),
        sqlc.arg(created_at), sqlc.arg(created_by), sqlc.arg(expires_at));

-- name: GetAPIKeyByPrefix :one
SELECT k.id, k.principal_id, k.key_hash, k.expires_at, k.revoked_at, p.kind, p.status
FROM studio.api_keys k
JOIN studio.principals p ON p.id = k.principal_id
WHERE k.prefix = sqlc.arg(prefix);

-- name: GetPrincipalForKey :one
SELECT id, kind, status FROM studio.principals WHERE id = sqlc.arg(id);

-- name: RevokeAPIKey :one
UPDATE studio.api_keys
SET revoked_at = sqlc.arg(now)
WHERE id = sqlc.arg(id) AND revoked_at IS NULL
RETURNING prefix, principal_id;

-- name: CreatePrincipal :exec
INSERT INTO studio.principals (id, kind, handle, created_by)
VALUES (sqlc.arg(id), sqlc.arg(kind), sqlc.arg(handle), sqlc.arg(created_by));

-- name: CreateUser :exec
INSERT INTO studio.users (principal_id, email, display_name, password_hash, global_role, created_by)
VALUES (sqlc.arg(principal_id), sqlc.arg(email)::text, sqlc.arg(display_name), sqlc.arg(password_hash),
        sqlc.arg(global_role), sqlc.arg(created_by));

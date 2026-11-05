-- User and service-account administration store (M4). Every statement runs as
-- studio_app and uses only the columns that role is granted (plan 6.3,
-- "Users and service accounts (admin)" and "reads: S of the relevant tables").

-- name: CreatePrincipal :exec
INSERT INTO studio.principals (id, kind, handle, created_by)
VALUES (sqlc.arg(id), sqlc.arg(kind), sqlc.arg(handle), sqlc.arg(created_by));

-- name: CreateUser :exec
INSERT INTO studio.users (principal_id, email, display_name, password_hash, global_role, created_at, created_by)
VALUES (sqlc.arg(principal_id), sqlc.arg(email)::text, sqlc.arg(display_name), sqlc.arg(password_hash),
        sqlc.arg(global_role), sqlc.arg(created_at), sqlc.arg(created_by));

-- name: CreateServiceAccount :exec
INSERT INTO studio.service_accounts (principal_id, description, global_role, created_at, created_by)
VALUES (sqlc.arg(principal_id), sqlc.arg(description), sqlc.arg(global_role), sqlc.arg(created_at),
        sqlc.arg(created_by));

-- name: GetUser :one
SELECT u.principal_id, u.email::text AS email, u.display_name, u.global_role, p.status, u.created_at
FROM studio.users u
JOIN studio.principals p ON p.id = u.principal_id
WHERE u.principal_id = sqlc.arg(id);

-- name: ListUsers :many
-- Cursor pagination in id order (UUIDv7, so creation order).
SELECT u.principal_id, u.email::text AS email, u.display_name, u.global_role, p.status, u.created_at
FROM studio.users u
JOIN studio.principals p ON p.id = u.principal_id
WHERE u.principal_id > sqlc.arg(after)
ORDER BY u.principal_id
LIMIT sqlc.arg(row_limit);

-- name: UpdateUser :execrows
-- A null argument leaves the column unchanged.
UPDATE studio.users
SET display_name  = COALESCE(sqlc.narg(display_name), display_name),
    global_role   = COALESCE(sqlc.narg(global_role), global_role),
    password_hash = COALESCE(sqlc.narg(password_hash), password_hash)
WHERE principal_id = sqlc.arg(id);

-- name: DisablePrincipal :execrows
UPDATE studio.principals
SET status = 'disabled'
WHERE id = sqlc.arg(id) AND kind = sqlc.arg(kind) AND status = 'active';

-- name: RevokePrincipalSessions :execrows
UPDATE studio.sessions
SET revoked_at = sqlc.arg(now)
WHERE principal_id = sqlc.arg(principal_id) AND revoked_at IS NULL;

-- name: ListServiceAccounts :many
SELECT sa.principal_id, sa.description, sa.global_role, p.status, sa.created_at
FROM studio.service_accounts sa
JOIN studio.principals p ON p.id = sa.principal_id
WHERE sa.principal_id > sqlc.arg(after)
ORDER BY sa.principal_id
LIMIT sqlc.arg(row_limit);

-- name: GetPrincipalKind :one
SELECT kind FROM studio.principals WHERE id = sqlc.arg(id);

-- name: ListAPIKeys :many
-- Never selects key_hash.
SELECT id, prefix, created_at, created_by, expires_at, revoked_at
FROM studio.api_keys
WHERE principal_id = sqlc.arg(principal_id)
ORDER BY id;

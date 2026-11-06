-- Blob store queries (M5, plan 11). Run as studio_app (upload) or
-- studio_worker (sweep, audit); each query uses only that role's grants.

-- name: InsertUpload :exec
-- Transaction 1: record the upload before any byte is written.
INSERT INTO studio.blob_uploads (id, staging_key, created_by)
-- staging_key must equal id::text; the database CHECK enforces it.
VALUES (sqlc.arg(id), sqlc.arg(staging_key), sqlc.arg(created_by));

-- name: GetUpload :one
-- Locks the upload row first; blob.sweep locks rows before per-sha keys too.
SELECT state, staging_key FROM studio.blob_uploads WHERE id = sqlc.arg(id) FOR UPDATE;

-- name: AbandonUpload :execrows
UPDATE studio.blob_uploads SET state = 'abandoned'
WHERE id = sqlc.arg(id) AND state = 'staging';

-- name: MarkPromoted :execrows
UPDATE studio.blob_uploads
SET state = 'promoted', sha256 = sqlc.arg(sha256), size = sqlc.arg(size), promoted_at = now()
WHERE id = sqlc.arg(id) AND state = 'staging';

-- name: MarkDeduplicated :execrows
UPDATE studio.blob_uploads
SET state = 'deduplicated', sha256 = sqlc.arg(sha256), size = sqlc.arg(size)
WHERE id = sqlc.arg(id) AND state = 'staging';

-- name: LockBlobKey :exec
-- pg_advisory_xact_lock(blob_key(sha256)) of plan 11, in the two-key space.
SELECT pg_advisory_xact_lock(sqlc.arg(namespace)::int, sqlc.arg(key)::int);

-- name: TryLockBackupGC :one
-- The exclusive side of BACKUP_GC (plan 11, 13): backups hold it shared.
SELECT pg_try_advisory_xact_lock(sqlc.arg(namespace)::int, sqlc.arg(key)::int);

-- name: GetBlobBySHA256 :one
SELECT id, sha256, size_bytes, media_type FROM studio.blob_objects WHERE sha256 = sqlc.arg(sha256);

-- name: InsertBlob :execrows
INSERT INTO studio.blob_objects (id, sha256, size_bytes, media_type, created_by)
VALUES (sqlc.arg(id), sqlc.arg(sha256), sqlc.arg(size_bytes), sqlc.arg(media_type), sqlc.arg(created_by))
ON CONFLICT (sha256) DO NOTHING;

-- name: BlobExists :one
SELECT EXISTS (SELECT 1 FROM studio.blob_objects WHERE sha256 = sqlc.arg(sha256));

-- name: ListExpiredUploads :many
-- Uploads still staging after their expiry. SKIP LOCKED leaves an upload that
-- is being committed right now to that commit.
SELECT id, staging_key FROM studio.blob_uploads
WHERE state = 'staging' AND expires_at < sqlc.arg(now)::timestamptz
ORDER BY expires_at
FOR UPDATE SKIP LOCKED;

-- name: GetUploadByStagingKey :one
SELECT state, expires_at FROM studio.blob_uploads WHERE staging_key = sqlc.arg(staging_key);

-- name: ListBlobs :many
SELECT id, sha256 FROM studio.blob_objects ORDER BY id;

-- name: InsertHealthEvent :execrows
-- The first detection of a kind for a blob is kept; re-detection is a no-op.
-- No conflict target: a target would need SELECT on its columns, and the
-- worker holds only INSERT here (X4). The only other unique key is the
-- generated id, so this skips exactly the (blob_object_id, kind) duplicates.
INSERT INTO studio.blob_health_events (blob_object_id, kind, details, created_by)
VALUES (sqlc.arg(blob_object_id), sqlc.arg(kind), sqlc.arg(details), sqlc.arg(created_by))
ON CONFLICT DO NOTHING;

-- name: CountHealthEvents :one
SELECT count(*) FROM studio.blob_health_events;

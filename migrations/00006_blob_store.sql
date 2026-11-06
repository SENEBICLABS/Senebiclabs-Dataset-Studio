-- M5 blob store: upload staging, the committed-blob ledger and blob health
-- events (plan 5.3 and 11). Forward-only: never edit after merge.
--
-- Committed blobs are immutable and are never deleted in Phase 1 (plan 11).
-- These rules are enforced here, in the database, not only in Go.

-- +goose Up

-- Rejects every UPDATE, DELETE or TRUNCATE on the table it is attached to.
-- +goose StatementBegin
CREATE FUNCTION studio.forbid_modification() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog
AS $$
BEGIN
    RAISE EXCEPTION '%: % is not allowed', TG_TABLE_NAME, TG_OP
        USING ERRCODE = 'restrict_violation';
END;
$$;
-- +goose StatementEnd

-- Uploads in progress (plan 11, transaction 1). staging_key is the upload id,
-- so it can never name a path outside the staging directory.
CREATE TABLE studio.blob_uploads (
    id          uuid        NOT NULL DEFAULT uuidv7(),
    state       text        NOT NULL DEFAULT 'staging',
    staging_key text        NOT NULL,
    sha256      bytea,
    size        bigint,
    expires_at  timestamptz NOT NULL DEFAULT now() + interval '24 hours',
    promoted_at timestamptz,
    created_at  timestamptz NOT NULL DEFAULT now(),
    created_by  uuid        NOT NULL,

    CONSTRAINT blob_uploads_pkey PRIMARY KEY (id),
    CONSTRAINT blob_uploads_staging_key_key UNIQUE (staging_key),
    CONSTRAINT blob_uploads_created_by_fkey FOREIGN KEY (created_by) REFERENCES studio.principals (id),
    CONSTRAINT blob_uploads_state_check CHECK (state IN ('staging', 'promoted', 'abandoned', 'deduplicated')),
    CONSTRAINT blob_uploads_staging_key_is_id CHECK (staging_key = id::text),
    CONSTRAINT blob_uploads_sha256_length CHECK (length(sha256) = 32),
    CONSTRAINT blob_uploads_size_nonnegative CHECK (size >= 0),
    -- A committed upload knows its content; staging and abandoned ones may not.
    CONSTRAINT blob_uploads_committed_has_content
        CHECK ((state IN ('promoted', 'deduplicated')) <= (sha256 IS NOT NULL AND size IS NOT NULL)),
    CONSTRAINT blob_uploads_promoted_at_iff_promoted CHECK ((state = 'promoted') = (promoted_at IS NOT NULL)),
    CONSTRAINT blob_uploads_expiry_after_creation CHECK (expires_at > created_at)
);

CREATE INDEX blob_uploads_staging_expiry_idx ON studio.blob_uploads (expires_at) WHERE state = 'staging';

-- An upload leaves 'staging' exactly once, to a terminal state, and nothing
-- else about it ever changes.
-- +goose StatementBegin
CREATE FUNCTION studio.blob_uploads_guard_update() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog
AS $$
BEGIN
    IF OLD.state <> 'staging' THEN
        RAISE EXCEPTION 'blob_uploads: upload % is already %', OLD.id, OLD.state
            USING ERRCODE = 'restrict_violation';
    END IF;
    IF NEW.id IS DISTINCT FROM OLD.id
       OR NEW.staging_key IS DISTINCT FROM OLD.staging_key
       OR NEW.expires_at IS DISTINCT FROM OLD.expires_at
       OR NEW.created_at IS DISTINCT FROM OLD.created_at
       OR NEW.created_by IS DISTINCT FROM OLD.created_by THEN
        RAISE EXCEPTION 'blob_uploads: only state, sha256, size and promoted_at may change'
            USING ERRCODE = 'restrict_violation';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER blob_uploads_guard_update
    BEFORE UPDATE ON studio.blob_uploads
    FOR EACH ROW EXECUTE FUNCTION studio.blob_uploads_guard_update();

CREATE TRIGGER blob_uploads_no_delete
    BEFORE DELETE ON studio.blob_uploads
    FOR EACH ROW EXECUTE FUNCTION studio.forbid_modification();

CREATE TRIGGER blob_uploads_no_truncate
    BEFORE TRUNCATE ON studio.blob_uploads
    FOR EACH STATEMENT EXECUTE FUNCTION studio.forbid_modification();

-- Committed blobs only (plan 5.3): no state column. Every blob reference is a
-- foreign key to this table. Immutable.
CREATE TABLE studio.blob_objects (
    id         uuid        NOT NULL DEFAULT uuidv7(),
    sha256     bytea       NOT NULL,
    size_bytes bigint      NOT NULL,
    media_type text        NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    created_by uuid        NOT NULL,

    CONSTRAINT blob_objects_pkey PRIMARY KEY (id),
    CONSTRAINT blob_objects_sha256_key UNIQUE (sha256),
    CONSTRAINT blob_objects_created_by_fkey FOREIGN KEY (created_by) REFERENCES studio.principals (id),
    CONSTRAINT blob_objects_sha256_length CHECK (length(sha256) = 32),
    CONSTRAINT blob_objects_size_nonnegative CHECK (size_bytes >= 0),
    CONSTRAINT blob_objects_media_type_length CHECK (length(media_type) BETWEEN 1 AND 255)
);

CREATE TRIGGER blob_objects_immutable
    BEFORE UPDATE OR DELETE ON studio.blob_objects
    FOR EACH ROW EXECUTE FUNCTION studio.forbid_modification();

CREATE TRIGGER blob_objects_no_truncate
    BEFORE TRUNCATE ON studio.blob_objects
    FOR EACH STATEMENT EXECUTE FUNCTION studio.forbid_modification();

-- Missing or corrupt committed blobs, found by blob.audit or verify-restore
-- (plan 11). Append-only. One row per blob and kind: the first detection is
-- recorded, and re-detection is a no-op, which keeps blob.audit idempotent.
CREATE TABLE studio.blob_health_events (
    id             uuid        NOT NULL DEFAULT uuidv7(),
    blob_object_id uuid        NOT NULL,
    kind           text        NOT NULL,
    detected_at    timestamptz NOT NULL DEFAULT clock_timestamp(),
    details        jsonb       NOT NULL DEFAULT '{}'::jsonb,
    created_at     timestamptz NOT NULL DEFAULT now(),
    created_by     uuid        NOT NULL,

    CONSTRAINT blob_health_events_pkey PRIMARY KEY (id),
    CONSTRAINT blob_health_events_blob_kind_key UNIQUE (blob_object_id, kind),
    CONSTRAINT blob_health_events_blob_fkey FOREIGN KEY (blob_object_id) REFERENCES studio.blob_objects (id),
    CONSTRAINT blob_health_events_created_by_fkey FOREIGN KEY (created_by) REFERENCES studio.principals (id),
    CONSTRAINT blob_health_events_kind_check CHECK (kind IN ('missing', 'corrupt')),
    CONSTRAINT blob_health_events_details_object CHECK (jsonb_typeof(details) = 'object')
);

CREATE TRIGGER blob_health_events_append_only
    BEFORE UPDATE OR DELETE ON studio.blob_health_events
    FOR EACH ROW EXECUTE FUNCTION studio.forbid_modification();

CREATE TRIGGER blob_health_events_no_truncate
    BEFORE TRUNCATE ON studio.blob_health_events
    FOR EACH STATEMENT EXECUTE FUNCTION studio.forbid_modification();

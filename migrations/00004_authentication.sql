-- M3 authentication: sessions and API keys (plan 5.1, 14).
-- Forward-only: never edit after merge.
--
-- Only hashes are stored: SHA-256 of the session token or full API key.
-- System principals cannot authenticate (plan 7.1): sessions belong to users
-- only, API keys to users or service accounts only, enforced by composite
-- foreign keys on (principal_id, kind).

-- +goose Up

CREATE TABLE studio.sessions (
    id             uuid              NOT NULL DEFAULT uuidv7(),
    principal_id   uuid              NOT NULL,
    principal_kind studio.actor_kind NOT NULL DEFAULT 'user',
    token_hash     bytea             NOT NULL,
    created_at     timestamptz       NOT NULL DEFAULT now(),
    expires_at     timestamptz       NOT NULL,
    last_seen_at   timestamptz       NOT NULL DEFAULT now(),
    revoked_at     timestamptz,
    ip             inet,
    user_agent     text,

    CONSTRAINT sessions_pkey PRIMARY KEY (id),
    CONSTRAINT sessions_token_hash_key UNIQUE (token_hash),
    CONSTRAINT sessions_principal_fkey FOREIGN KEY (principal_id, principal_kind)
        REFERENCES studio.principals (id, kind),
    CONSTRAINT sessions_users_only CHECK (principal_kind = 'user'),
    CONSTRAINT sessions_token_hash_sha256 CHECK (length(token_hash) = 32),
    CONSTRAINT sessions_expiry_after_creation CHECK (expires_at > created_at),
    CONSTRAINT sessions_user_agent_length CHECK (length(user_agent) <= 512)
);

CREATE INDEX sessions_principal_idx ON studio.sessions (principal_id);

CREATE TABLE studio.api_keys (
    id             uuid              NOT NULL DEFAULT uuidv7(),
    principal_id   uuid              NOT NULL,
    principal_kind studio.actor_kind NOT NULL,
    prefix         text              NOT NULL,
    key_hash       bytea             NOT NULL,
    created_at     timestamptz       NOT NULL DEFAULT now(),
    created_by     uuid              NOT NULL,
    expires_at     timestamptz       NOT NULL,
    revoked_at     timestamptz,

    CONSTRAINT api_keys_pkey PRIMARY KEY (id),
    CONSTRAINT api_keys_prefix_key UNIQUE (prefix),
    CONSTRAINT api_keys_key_hash_key UNIQUE (key_hash),
    CONSTRAINT api_keys_principal_fkey FOREIGN KEY (principal_id, principal_kind)
        REFERENCES studio.principals (id, kind),
    CONSTRAINT api_keys_created_by_fkey FOREIGN KEY (created_by) REFERENCES studio.principals (id),
    CONSTRAINT api_keys_no_system_principals CHECK (principal_kind IN ('user', 'service_account')),
    CONSTRAINT api_keys_prefix_format CHECK (prefix ~ '^[a-z2-7]{8}$'),
    CONSTRAINT api_keys_key_hash_sha256 CHECK (length(key_hash) = 32),
    CONSTRAINT api_keys_expiry_after_creation CHECK (expires_at > created_at)
);

CREATE INDEX api_keys_principal_idx ON studio.api_keys (principal_id);

-- Credentials cannot be altered after issue: only last_seen_at (sessions) may
-- move, and revoked_at may be set once and never cleared or changed. This
-- backs the column grants with a rule no role can bypass.
-- +goose StatementBegin
CREATE FUNCTION studio.credentials_restrict_update() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog
AS $$
DECLARE
    old_row jsonb := to_jsonb(OLD) - 'revoked_at' - 'last_seen_at';
    new_row jsonb := to_jsonb(NEW) - 'revoked_at' - 'last_seen_at';
BEGIN
    IF old_row IS DISTINCT FROM new_row THEN
        RAISE EXCEPTION '%: only revocation (and session last_seen_at) may change', TG_TABLE_NAME
            USING ERRCODE = 'restrict_violation';
    END IF;
    IF OLD.revoked_at IS NOT NULL AND NEW.revoked_at IS DISTINCT FROM OLD.revoked_at THEN
        RAISE EXCEPTION '%: revocation is permanent', TG_TABLE_NAME
            USING ERRCODE = 'restrict_violation';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER sessions_restrict_update
    BEFORE UPDATE ON studio.sessions
    FOR EACH ROW EXECUTE FUNCTION studio.credentials_restrict_update();

CREATE TRIGGER api_keys_restrict_update
    BEFORE UPDATE ON studio.api_keys
    FOR EACH ROW EXECUTE FUNCTION studio.credentials_restrict_update();

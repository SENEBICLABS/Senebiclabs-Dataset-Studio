-- M1 foundation: principals, identity tables, installation, idempotency keys.
-- Plan sections 5.1, 6 and 7.1. Forward-only: never edit after merge.
--
-- Runs as studio_owner (the migrator connects and does SET ROLE studio_owner),
-- so every object created here is owned by studio_owner and no login role can
-- alter or disable it.

-- +goose Up

-- Functions created from now on are not executable by PUBLIC. This is the
-- global default (not per-schema), because per-schema defaults can only add.
ALTER DEFAULT PRIVILEGES REVOKE EXECUTE ON FUNCTIONS FROM PUBLIC;

CREATE EXTENSION IF NOT EXISTS citext WITH SCHEMA studio;

-- Shared actor-kind enum. principals reject 'anonymous'; audit_events (M2)
-- uses the full set.
CREATE TYPE studio.actor_kind AS ENUM ('user', 'service_account', 'system', 'anonymous');

CREATE TABLE studio.principals (
    id         uuid              NOT NULL DEFAULT uuidv7(),
    kind       studio.actor_kind NOT NULL,
    handle     text              NOT NULL,
    status     text              NOT NULL DEFAULT 'active',
    created_at timestamptz       NOT NULL DEFAULT now(),
    created_by uuid              NOT NULL,

    CONSTRAINT principals_pkey PRIMARY KEY (id),
    CONSTRAINT principals_handle_key UNIQUE (handle),
    CONSTRAINT principals_id_kind_key UNIQUE (id, kind),
    CONSTRAINT principals_created_by_fkey FOREIGN KEY (created_by) REFERENCES studio.principals (id),
    CONSTRAINT principals_kind_not_anonymous CHECK (kind <> 'anonymous'),
    CONSTRAINT principals_status_check CHECK (status IN ('active', 'disabled')),

    -- Root principal (plan 7.1). Written as equivalences so they hold in both
    -- directions: the root must self-reference and no other row may; the root
    -- lives at the fixed id and no other row may.
    CONSTRAINT principals_root_self_reference
        CHECK ((handle = 'system:migration') = (created_by = id)),
    CONSTRAINT principals_root_fixed_id
        CHECK ((handle = 'system:migration') = (id = '00000000-0000-7000-8000-000000000001'::uuid)),
    CONSTRAINT principals_root_is_system
        CHECK (handle <> 'system:migration' OR kind = 'system')
);

-- +goose StatementBegin
CREATE FUNCTION studio.principals_forbid_identity_update() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog
AS $$
BEGIN
    IF NEW.id IS DISTINCT FROM OLD.id
       OR NEW.handle IS DISTINCT FROM OLD.handle
       OR NEW.kind IS DISTINCT FROM OLD.kind
       OR NEW.created_by IS DISTINCT FROM OLD.created_by THEN
        RAISE EXCEPTION 'principals: id, handle, kind and created_by are immutable'
            USING ERRCODE = 'restrict_violation';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER principals_forbid_identity_update
    BEFORE UPDATE ON studio.principals
    FOR EACH ROW EXECUTE FUNCTION studio.principals_forbid_identity_update();

-- Credential-bearing identities. The composite foreign key on
-- (principal_id, kind) plus the kind CHECK keeps system principals (and any
-- principal of another kind) out of these tables, so system principals cannot
-- authenticate (plan 7.1). Authentication itself is M3.
CREATE TABLE studio.users (
    principal_id  uuid              NOT NULL,
    kind          studio.actor_kind NOT NULL DEFAULT 'user',
    email         studio.citext     NOT NULL,
    display_name  text              NOT NULL,
    password_hash text,
    global_role   text              NOT NULL DEFAULT 'member',
    created_at    timestamptz       NOT NULL DEFAULT now(),
    created_by    uuid              NOT NULL,

    CONSTRAINT users_pkey PRIMARY KEY (principal_id),
    CONSTRAINT users_email_key UNIQUE (email),
    CONSTRAINT users_principal_fkey FOREIGN KEY (principal_id, kind) REFERENCES studio.principals (id, kind),
    CONSTRAINT users_created_by_fkey FOREIGN KEY (created_by) REFERENCES studio.principals (id),
    CONSTRAINT users_kind_check CHECK (kind = 'user'),
    CONSTRAINT users_global_role_check CHECK (global_role IN ('platform_admin', 'registry_admin', 'member'))
);

CREATE TABLE studio.service_accounts (
    principal_id uuid              NOT NULL,
    kind         studio.actor_kind NOT NULL DEFAULT 'service_account',
    description  text              NOT NULL DEFAULT '',
    global_role  text              NOT NULL DEFAULT 'member',
    created_at   timestamptz       NOT NULL DEFAULT now(),
    created_by   uuid              NOT NULL,

    CONSTRAINT service_accounts_pkey PRIMARY KEY (principal_id),
    CONSTRAINT service_accounts_principal_fkey FOREIGN KEY (principal_id, kind) REFERENCES studio.principals (id, kind),
    CONSTRAINT service_accounts_created_by_fkey FOREIGN KEY (created_by) REFERENCES studio.principals (id),
    CONSTRAINT service_accounts_kind_check CHECK (kind = 'service_account'),
    CONSTRAINT service_accounts_global_role_check CHECK (global_role IN ('platform_admin', 'registry_admin', 'member'))
);

-- Singleton identity of this installation; recorded in every manifest.
CREATE TABLE studio.installation (
    singleton       boolean     NOT NULL DEFAULT true,
    installation_id uuid        NOT NULL DEFAULT gen_random_uuid(),
    created_at      timestamptz NOT NULL DEFAULT now(),
    created_by      uuid        NOT NULL,

    CONSTRAINT installation_pkey PRIMARY KEY (singleton),
    CONSTRAINT installation_singleton_check CHECK (singleton),
    CONSTRAINT installation_installation_id_key UNIQUE (installation_id),
    CONSTRAINT installation_created_by_fkey FOREIGN KEY (created_by) REFERENCES studio.principals (id)
);

CREATE TABLE studio.idempotency_keys (
    principal_id    uuid        NOT NULL,
    key             text        NOT NULL,
    request_sha256  bytea       NOT NULL,
    response_status integer     NOT NULL,
    response_body   bytea       NOT NULL,
    expires_at      timestamptz NOT NULL,
    created_at      timestamptz NOT NULL DEFAULT now(),
    created_by      uuid        NOT NULL,

    CONSTRAINT idempotency_keys_pkey PRIMARY KEY (principal_id, key),
    CONSTRAINT idempotency_keys_principal_fkey FOREIGN KEY (principal_id) REFERENCES studio.principals (id),
    CONSTRAINT idempotency_keys_created_by_fkey FOREIGN KEY (created_by) REFERENCES studio.principals (id),
    CONSTRAINT idempotency_keys_key_length CHECK (length(key) BETWEEN 1 AND 255),
    CONSTRAINT idempotency_keys_request_sha256_length CHECK (length(request_sha256) = 32),
    CONSTRAINT idempotency_keys_response_status_check CHECK (response_status BETWEEN 100 AND 599),
    CONSTRAINT idempotency_keys_response_body_cap CHECK (length(response_body) <= 65536)
);

CREATE INDEX idempotency_keys_expires_at_idx ON studio.idempotency_keys (expires_at);

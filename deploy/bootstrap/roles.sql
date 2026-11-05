-- Superuser-only bootstrap: cluster roles (plan 6.1). Creates no rows in any
-- application table. Idempotent: safe to run again; it re-asserts attributes,
-- memberships and per-role settings. Passwords are set separately by
-- `studioctl db bootstrap` as SCRAM verifiers.

DO $$
DECLARE
    r record;
BEGIN
    FOR r IN SELECT * FROM (VALUES
            ('studio_owner',    false),
            ('studio_migrator', true),
            ('studio_app',      true),
            ('studio_worker',   true),
            ('studio_backup',   true),
            ('studio_readonly', true)) AS t(name, can_login)
    LOOP
        IF NOT EXISTS (SELECT 1 FROM pg_catalog.pg_roles WHERE rolname = r.name) THEN
            EXECUTE format('CREATE ROLE %I', r.name);
        END IF;
        EXECUTE format(
            'ALTER ROLE %I %s NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS',
            r.name, CASE WHEN r.can_login THEN 'LOGIN' ELSE 'NOLOGIN' END);
    END LOOP;
END $$;

-- The migrator acts as the owner only through an explicit SET ROLE; it
-- inherits none of the owner's privileges.
GRANT studio_owner TO studio_migrator WITH INHERIT FALSE, SET TRUE;

-- Backup reads everything and writes nothing.
GRANT pg_read_all_data TO studio_backup;

-- search_path is pinned per role (plan 6.1).
ALTER ROLE studio_owner    SET search_path = studio;
ALTER ROLE studio_migrator SET search_path = studio;
ALTER ROLE studio_app      SET search_path = studio;
ALTER ROLE studio_worker   SET search_path = studio;
ALTER ROLE studio_backup   SET search_path = studio;
ALTER ROLE studio_readonly SET search_path = studio;

-- Per-role timeouts (plan 6.1).
ALTER ROLE studio_app    SET statement_timeout = '15s';
ALTER ROLE studio_app    SET idle_in_transaction_session_timeout = '30s';
ALTER ROLE studio_worker SET statement_timeout = '5min';
ALTER ROLE studio_worker SET idle_in_transaction_session_timeout = '1min';
ALTER ROLE studio_backup SET statement_timeout = 0;
ALTER ROLE studio_backup SET idle_in_transaction_session_timeout = 0;

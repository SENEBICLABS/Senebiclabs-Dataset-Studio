-- M6 jobs: River v0.49.0's PostgreSQL schema, carried as our own goose
-- migration (plan 3.1, 5.10 and R8). Forward-only: never edit after merge.
--
-- This is the end state of River's own "main" migration line, versions 001
-- to 008, at the pinned v0.49.0, written as one migration in the studio
-- schema. It is not a replay of River's history: the enum is created with its
-- final values instead of adding 'pending' later, so no enum value is added
-- and used in one transaction. The integration test
-- TestRiverSchemaMatchesRiverMigrations applies River's own migrations to a
-- scratch schema and asserts the two catalogs are identical, so any drift from
-- the pinned version fails the build.
--
-- Runs as studio_owner: every River object is owned by the non-login owner,
-- and River's login roles receive only the grants in internal/dbroles (plan
-- 6.3, R5 and R7). River tables are exempt from created_by (plan 7.1).

-- +goose Up

CREATE TYPE studio.river_job_state AS ENUM (
    'available',
    'cancelled',
    'completed',
    'discarded',
    'pending',
    'retryable',
    'running',
    'scheduled'
);

-- River's migration bookkeeping (line, version). The rows inserted below
-- record that the main line is at version 8, so River's tooling sees the
-- schema as current. River's migrator is never run against this database.
-- The "1" suffixes are the names PostgreSQL gives these constraints when
-- River's migration 005 recreates the table next to the renamed original;
-- they are spelled out so the catalog matches River's exactly.
CREATE TABLE studio.river_migration (
    line       text        NOT NULL,
    version    bigint      CONSTRAINT river_migration_version_not_null1 NOT NULL,
    created_at timestamptz CONSTRAINT river_migration_created_at_not_null1 NOT NULL DEFAULT now(),
    CONSTRAINT line_length CHECK (char_length(line) > 0 AND char_length(line) < 128),
    CONSTRAINT version_gte_1 CHECK (version >= 1),
    CONSTRAINT river_migration_pkey1 PRIMARY KEY (line, version)
);

CREATE TABLE studio.river_job (
    id            bigserial              PRIMARY KEY,
    state         studio.river_job_state NOT NULL DEFAULT 'available',
    attempt       smallint               NOT NULL DEFAULT 0,
    max_attempts  smallint               NOT NULL DEFAULT 25,
    attempted_at  timestamptz,
    created_at    timestamptz            NOT NULL DEFAULT now(),
    finalized_at  timestamptz,
    scheduled_at  timestamptz            NOT NULL DEFAULT now(),
    priority      smallint               NOT NULL DEFAULT 1,
    args          jsonb                  NOT NULL,
    attempted_by  text[],
    errors        jsonb[],
    kind          text                   NOT NULL,
    metadata      jsonb                  NOT NULL DEFAULT '{}',
    queue         text                   NOT NULL DEFAULT 'default',
    tags          varchar(255)[]         NOT NULL DEFAULT '{}',
    unique_key    bytea,
    unique_states bit(8),

    CONSTRAINT max_attempts_is_positive CHECK (max_attempts > 0),
    CONSTRAINT priority_in_range CHECK (priority >= 1 AND priority <= 4),
    CONSTRAINT queue_length CHECK (char_length(queue) > 0 AND char_length(queue) < 128),
    CONSTRAINT kind_length CHECK (char_length(kind) > 0 AND char_length(kind) < 128),
    CONSTRAINT finalized_or_finalized_at_null CHECK (
        (finalized_at IS NULL AND state NOT IN ('cancelled', 'completed', 'discarded')) OR
        (finalized_at IS NOT NULL AND state IN ('cancelled', 'completed', 'discarded'))
    )
);

CREATE INDEX river_job_kind ON studio.river_job USING btree (kind);
CREATE INDEX river_job_state_and_finalized_at_index ON studio.river_job USING btree (state, finalized_at)
    WHERE finalized_at IS NOT NULL;
CREATE INDEX river_job_prioritized_fetching_index ON studio.river_job USING btree (state, queue, priority, scheduled_at, id);
CREATE INDEX river_job_args_index ON studio.river_job USING gin (args);
CREATE INDEX river_job_metadata_index ON studio.river_job USING gin (metadata);

-- +goose StatementBegin
CREATE FUNCTION studio.river_job_state_in_bitmask(bitmask bit(8), state studio.river_job_state)
RETURNS boolean
LANGUAGE sql
IMMUTABLE
AS $$
    SELECT CASE state
        WHEN 'available' THEN get_bit(bitmask, 7)
        WHEN 'cancelled' THEN get_bit(bitmask, 6)
        WHEN 'completed' THEN get_bit(bitmask, 5)
        WHEN 'discarded' THEN get_bit(bitmask, 4)
        WHEN 'pending'   THEN get_bit(bitmask, 3)
        WHEN 'retryable' THEN get_bit(bitmask, 2)
        WHEN 'running'   THEN get_bit(bitmask, 1)
        WHEN 'scheduled' THEN get_bit(bitmask, 0)
        ELSE 0
    END = 1;
$$;
-- +goose StatementEnd

CREATE UNIQUE INDEX river_job_unique_idx ON studio.river_job (unique_key)
    WHERE unique_key IS NOT NULL
      AND unique_states IS NOT NULL
      AND studio.river_job_state_in_bitmask(unique_states, state);

CREATE UNLOGGED TABLE studio.river_leader (
    elected_at timestamptz NOT NULL,
    expires_at timestamptz NOT NULL,
    leader_id  text        NOT NULL,
    name       text        PRIMARY KEY DEFAULT 'default',
    CONSTRAINT name_length CHECK (name = 'default'),
    CONSTRAINT leader_id_length CHECK (char_length(leader_id) > 0 AND char_length(leader_id) < 128)
);

CREATE TABLE studio.river_queue (
    name       text        PRIMARY KEY NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    metadata   jsonb       NOT NULL DEFAULT '{}'::jsonb,
    paused_at  timestamptz,
    updated_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE studio.river_notification (
    id         bigserial   PRIMARY KEY,
    created_at timestamptz NOT NULL DEFAULT now(),
    payload    text        NOT NULL,
    topic      text        NOT NULL,
    CONSTRAINT topic_length CHECK (length(topic) > 0 AND length(topic) < 128)
);

CREATE INDEX river_notification_created_at_idx ON studio.river_notification (created_at);
CREATE INDEX river_notification_topic_id_idx ON studio.river_notification (topic, id);

INSERT INTO studio.river_migration (line, version)
SELECT 'main', v FROM generate_series(1, 8) AS v;

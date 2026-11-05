-- M2 audit core: the append-only audit_events table (plan 5.2, 7, 8).
-- Forward-only: never edit after merge.

-- +goose Up

CREATE TABLE studio.audit_events (
    id                        uuid              NOT NULL DEFAULT uuidv7(),
    occurred_at               timestamptz       NOT NULL DEFAULT clock_timestamp(),
    tx_id                     bigint            NOT NULL DEFAULT txid_current(),
    action                    text              NOT NULL,
    action_class              text              NOT NULL,
    outcome                   text              NOT NULL,
    actor_principal_id        uuid,
    actor_kind                studio.actor_kind NOT NULL,
    on_behalf_of_principal_id uuid,
    auth_method               text,
    target_type               text,
    target_id                 text,
    project_id                uuid,
    dataset_id                uuid,
    request_id                text,
    job_id                    bigint,
    job_attempt               integer,
    job_trigger               text,
    causation_event_id        uuid,
    causation_outcome         text,
    details                   jsonb             NOT NULL DEFAULT '{}'::jsonb,
    details_version           integer           NOT NULL DEFAULT 1,

    CONSTRAINT audit_events_pkey PRIMARY KEY (id),
    -- Target of the causation foreign key below (plan 8.2).
    CONSTRAINT audit_events_id_outcome_key UNIQUE (id, outcome),

    CONSTRAINT audit_events_action_not_empty CHECK (length(action) > 0),
    CONSTRAINT audit_events_action_class_check CHECK (action_class IN ('command', 'job_execution', 'security')),
    CONSTRAINT audit_events_outcome_check CHECK (outcome IN ('succeeded', 'denied', 'failed')),
    CONSTRAINT audit_events_details_object CHECK (jsonb_typeof(details) = 'object'),
    CONSTRAINT audit_events_details_version_check CHECK (details_version >= 1),

    -- Actor kind must be the referenced principal's actual kind (plan 8.2).
    -- MATCH SIMPLE: unchecked when the actor is null (anonymous).
    CONSTRAINT audit_events_actor_fkey FOREIGN KEY (actor_principal_id, actor_kind)
        REFERENCES studio.principals (id, kind) MATCH SIMPLE,
    -- Anonymous actor (plan 7.1): null actor exactly when anonymous, and only
    -- for failed logins.
    CONSTRAINT audit_events_anonymous_actor CHECK ((actor_kind = 'anonymous') = (actor_principal_id IS NULL)),
    CONSTRAINT audit_events_anonymous_only_login_failed CHECK (actor_kind <> 'anonymous' OR action = 'auth.login_failed'),

    -- Accountable principal (plan 7.2): on_behalf_of only for system actors.
    CONSTRAINT audit_events_on_behalf_of_fkey FOREIGN KEY (on_behalf_of_principal_id)
        REFERENCES studio.principals (id),
    CONSTRAINT audit_events_on_behalf_of_system_only
        CHECK (on_behalf_of_principal_id IS NULL OR actor_kind = 'system'),

    -- Job-execution constraints (plan 8.2; tests JA1-JA9, plan 17.4.1).
    CONSTRAINT audit_events_job_id_iff_job_execution                                   -- JA1
        CHECK ((action_class = 'job_execution') = (job_id IS NOT NULL)),
    CONSTRAINT audit_events_job_fields_together                                        -- JA2
        CHECK (num_nulls(job_id, job_attempt, job_trigger) IN (0, 3)),
    CONSTRAINT audit_events_job_attempt_positive CHECK (job_attempt >= 1),            -- JA3
    CONSTRAINT audit_events_job_trigger_check CHECK (job_trigger IN ('command', 'periodic')),
    CONSTRAINT audit_events_job_execution_by_worker                                    -- JA4
        CHECK ((action_class = 'job_execution')
               = (actor_principal_id IS NOT DISTINCT FROM '00000000-0000-7000-8000-000000000003'::uuid)),
    CONSTRAINT audit_events_command_job_has_causation                                  -- JA5
        CHECK (job_trigger IS DISTINCT FROM 'command'
               OR (causation_event_id IS NOT NULL AND on_behalf_of_principal_id IS NOT NULL)),
    CONSTRAINT audit_events_periodic_job_has_no_causation                              -- JA6
        CHECK (job_trigger IS DISTINCT FROM 'periodic'
               OR (causation_event_id IS NULL AND on_behalf_of_principal_id IS NULL)),
    CONSTRAINT audit_events_causation_fkey FOREIGN KEY (causation_event_id, causation_outcome)  -- JA7
        REFERENCES studio.audit_events (id, outcome),
    CONSTRAINT audit_events_causation_outcome_succeeded
        CHECK (causation_outcome = 'succeeded'),
    CONSTRAINT audit_events_causation_pair
        CHECK (num_nulls(causation_event_id, causation_outcome) IN (0, 2))
);

-- JA9: at most one job_execution event per job.
CREATE UNIQUE INDEX audit_events_job_execution_job_id_key
    ON studio.audit_events (job_id) WHERE action_class = 'job_execution';

CREATE INDEX audit_events_occurred_at_idx ON studio.audit_events (occurred_at);
CREATE INDEX audit_events_target_idx ON studio.audit_events (target_type, target_id);
CREATE INDEX audit_events_actor_idx ON studio.audit_events (actor_principal_id);

-- Append-only: no UPDATE, DELETE or TRUNCATE for anyone, including the owner
-- and superusers (plan 8.1, M2 acceptance: "UPDATE and DELETE on audit rows
-- fail"). Grants additionally withhold these privileges from every role.
-- +goose StatementBegin
CREATE FUNCTION studio.audit_events_append_only() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog
AS $$
BEGIN
    RAISE EXCEPTION 'audit_events is append-only: % is not allowed', TG_OP
        USING ERRCODE = 'restrict_violation';
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER audit_events_no_update_delete
    BEFORE UPDATE OR DELETE ON studio.audit_events
    FOR EACH ROW EXECUTE FUNCTION studio.audit_events_append_only();

CREATE TRIGGER audit_events_no_truncate
    BEFORE TRUNCATE ON studio.audit_events
    FOR EACH STATEMENT EXECUTE FUNCTION studio.audit_events_append_only();

-- Server-assigned time and transaction: callers cannot back-date an event,
-- which the causation ordering check (JA8) relies on.
-- +goose StatementBegin
CREATE FUNCTION studio.audit_events_assign_server_fields() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog
AS $$
BEGIN
    NEW.occurred_at := clock_timestamp();
    NEW.tx_id := txid_current();
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER audit_events_10_assign_server_fields
    BEFORE INSERT ON studio.audit_events
    FOR EACH ROW EXECUTE FUNCTION studio.audit_events_assign_server_fields();

-- JA8: the causing event must have enqueued this job, have occurred earlier,
-- and pass on its accountable principal. SECURITY DEFINER so roles that may
-- only INSERT (studio_worker) can still be checked against the causing row;
-- it reads one row and exposes nothing.
-- +goose StatementBegin
CREATE FUNCTION studio.audit_events_check_causation() RETURNS trigger
    LANGUAGE plpgsql
    SECURITY DEFINER
    SET search_path = pg_catalog
AS $$
DECLARE
    cause record;
BEGIN
    IF NEW.causation_event_id IS NULL THEN
        RETURN NEW;
    END IF;
    SELECT occurred_at, details, actor_principal_id, on_behalf_of_principal_id
      INTO cause
      FROM studio.audit_events
     WHERE id = NEW.causation_event_id;
    IF NOT FOUND THEN
        -- The foreign key reports this case.
        RETURN NEW;
    END IF;
    IF NEW.job_id IS NULL
       OR NOT coalesce(cause.details -> 'enqueued_jobs' @> to_jsonb(NEW.job_id), false) THEN
        RAISE EXCEPTION 'audit causation: event % did not enqueue job %', NEW.causation_event_id, NEW.job_id
            USING ERRCODE = 'check_violation', CONSTRAINT = 'audit_events_causation_enqueued_job';
    END IF;
    IF NOT (cause.occurred_at < NEW.occurred_at) THEN
        RAISE EXCEPTION 'audit causation: event % did not occur earlier', NEW.causation_event_id
            USING ERRCODE = 'check_violation', CONSTRAINT = 'audit_events_causation_earlier';
    END IF;
    -- A missing on_behalf_of is left to the CHECKs (JA5 for command-triggered
    -- jobs, JA6 for periodic ones) so each violation is reported by its own
    -- constraint; every such row is still rejected.
    IF NEW.on_behalf_of_principal_id IS NOT NULL
       AND NEW.on_behalf_of_principal_id IS DISTINCT FROM
           coalesce(cause.on_behalf_of_principal_id, cause.actor_principal_id) THEN
        RAISE EXCEPTION 'audit causation: on_behalf_of must be inherited from event %', NEW.causation_event_id
            USING ERRCODE = 'check_violation', CONSTRAINT = 'audit_events_causation_on_behalf_of';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

-- Fires after the server-field trigger (triggers run in name order).
CREATE TRIGGER audit_events_20_check_causation
    BEFORE INSERT ON studio.audit_events
    FOR EACH ROW EXECUTE FUNCTION studio.audit_events_check_causation();

-- Seeded rows get one system.seeded event (plan 8.1). The seed itself is
-- migration 00002; this records it now that audit_events exists.
INSERT INTO studio.audit_events (action, action_class, outcome, actor_principal_id, actor_kind, auth_method, details)
VALUES (
    'system.seeded', 'command', 'succeeded',
    '00000000-0000-7000-8000-000000000001', 'system', 'migration',
    jsonb_build_object(
        'principals', jsonb_build_array('system:migration', 'system:cli', 'system:worker'),
        'installation', true,
        'migration', 2
    )
);

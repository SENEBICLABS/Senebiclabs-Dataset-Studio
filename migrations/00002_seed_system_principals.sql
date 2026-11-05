-- M1 seed: the three system principals and the installation row (plan 7.1).
-- Forward-only: never edit after merge.
--
-- system:migration is the root: the only principal whose created_by is itself.
-- The other system principals are created by it. System principals have no
-- users/service_accounts row, so they can never authenticate.
--
-- The system.seeded audit event (plan 8.1) is written once audit_events
-- exists (M2).

-- +goose Up

INSERT INTO studio.principals (id, kind, handle, created_by) VALUES
    ('00000000-0000-7000-8000-000000000001', 'system', 'system:migration', '00000000-0000-7000-8000-000000000001'),
    ('00000000-0000-7000-8000-000000000002', 'system', 'system:cli',       '00000000-0000-7000-8000-000000000001'),
    ('00000000-0000-7000-8000-000000000003', 'system', 'system:worker',    '00000000-0000-7000-8000-000000000001');

INSERT INTO studio.installation (created_by) VALUES ('00000000-0000-7000-8000-000000000001');

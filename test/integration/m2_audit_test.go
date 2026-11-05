//go:build integration

package integration

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/SENEBICLABS/Senebiclabs-Dataset-Studio/internal/audit"
	"github.com/SENEBICLABS/Senebiclabs-Dataset-Studio/internal/dbroles"
	"github.com/SENEBICLABS/Senebiclabs-Dataset-Studio/internal/principals"
	"github.com/SENEBICLABS/Senebiclabs-Dataset-Studio/internal/testdb"
)

const (
	userU    = "0190a6e4-0000-7000-8000-0000000000a1"
	userV    = "0190a6e4-0000-7000-8000-0000000000a2"
	workerID = principals.WorkerID
)

// testActions is the catalog the M2 tests write with; production actions
// arrive with the milestones that introduce them.
var testActions = audit.Catalog{
	"thing.created":       {Class: audit.Command, DetailsVersion: 1},
	"thing.read_denied":   {Class: audit.Security, DetailsVersion: 1},
	"job.thing_processed": {Class: audit.JobExecution, DetailsVersion: 1},
}

// auditDB returns a fresh database with two user principals, U and V.
func auditDB(t *testing.T) (*testdb.DB, *pgx.Conn) {
	t.Helper()
	db := testdb.New(t)
	admin := db.Connect(t, testdb.Admin)
	if _, err := admin.Exec(ctx(t), `
		INSERT INTO studio.principals (id, kind, handle, created_by) VALUES
			($1, 'user', 'user-u', $3), ($2, 'user', 'user-v', $3)`, userU, userV, principals.RootID); err != nil {
		t.Fatal(err)
	}
	return db, admin
}

func appPool(t *testing.T, db *testdb.DB) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(ctx(t), db.URL(dbroles.App))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func countEvents(t *testing.T, conn *pgx.Conn, where string, args ...any) int {
	t.Helper()
	var n int
	if err := conn.QueryRow(ctx(t), `SELECT count(*) FROM studio.audit_events WHERE `+where, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// insertCommand writes a succeeded command event by user U as the superuser
// and returns its id. enqueued lists the job ids it enqueued.
func insertCommand(t *testing.T, conn *pgx.Conn, enqueued ...int64) string {
	t.Helper()
	details := map[string]any{}
	if len(enqueued) > 0 {
		details["enqueued_jobs"] = enqueued
	}
	b, _ := json.Marshal(details)
	var id string
	if err := conn.QueryRow(ctx(t), `
		INSERT INTO studio.audit_events (action, action_class, outcome, actor_principal_id, actor_kind, details)
		VALUES ('thing.created', 'command', 'succeeded', $1, 'user', $2) RETURNING id`, userU, b).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

const insertJobSQL = `
	INSERT INTO studio.audit_events (action, action_class, outcome, actor_principal_id, actor_kind,
		on_behalf_of_principal_id, job_id, job_attempt, job_trigger, causation_event_id, causation_outcome)
	VALUES ($1, $2, 'succeeded', $3, $4::studio.actor_kind, $5, $6, $7, $8, $9, $10)`

type jobRow struct {
	action, class, actor, kind string
	onBehalf                   any
	jobID, attempt, trigger    any
	causation, causationOut    any
}

func validPeriodic(jobID int64) jobRow {
	return jobRow{action: "job.thing_processed", class: "job_execution", actor: workerID, kind: "system",
		jobID: jobID, attempt: 1, trigger: "periodic"}
}

func validCommandJob(jobID int64, cause string) jobRow {
	r := validPeriodic(jobID)
	r.trigger, r.onBehalf, r.causation, r.causationOut = "command", userU, cause, "succeeded"
	return r
}

func (r jobRow) args() []any {
	return []any{r.action, r.class, r.actor, r.kind, r.onBehalf, r.jobID, r.attempt, r.trigger, r.causation, r.causationOut}
}

// M2 acceptance: a rolled-back transaction leaves no event; a committed one
// leaves exactly one, with server-assigned time and transaction id.
func TestAuditRolledBackTransactionLeavesNoEvent(t *testing.T) {
	db, admin := auditDB(t)
	app := db.Connect(t, dbroles.App)
	w := audit.NewWriter(testActions)
	ev := audit.Event{Action: "thing.created", Outcome: audit.Succeeded, ActorPrincipalID: userU, ActorKind: audit.User,
		TargetType: "thing", TargetID: "rolled-back"}

	tx, err := app.Begin(ctx(t))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Record(ctx(t), tx, ev); err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(ctx(t)); err != nil {
		t.Fatal(err)
	}
	if n := countEvents(t, admin, `target_id = 'rolled-back'`); n != 0 {
		t.Fatalf("rolled-back command left %d events", n)
	}

	ev.TargetID = "committed"
	tx, err = app.Begin(ctx(t))
	if err != nil {
		t.Fatal(err)
	}
	id, err := w.Record(ctx(t), tx, ev)
	if err != nil {
		t.Fatal(err)
	}
	var txid int64
	if err := tx.QueryRow(ctx(t), `SELECT txid_current()`).Scan(&txid); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx(t)); err != nil {
		t.Fatal(err)
	}
	var gotTx int64
	var class string
	var version int
	if err := admin.QueryRow(ctx(t), `SELECT tx_id, action_class, details_version FROM studio.audit_events WHERE id = $1`, id).
		Scan(&gotTx, &class, &version); err != nil {
		t.Fatal(err)
	}
	if gotTx != txid || class != "command" || version != 1 {
		t.Errorf("stored tx_id %d (want %d), class %s, version %d", gotTx, txid, class, version)
	}
}

// Callers cannot back-date events: occurred_at is server-assigned.
func TestAuditOccurredAtIsServerAssigned(t *testing.T) {
	_, admin := auditDB(t)
	var occurred, now time.Time
	if err := admin.QueryRow(ctx(t), `
		INSERT INTO studio.audit_events (occurred_at, action, action_class, outcome, actor_principal_id, actor_kind)
		VALUES ('2000-01-01', 'thing.created', 'command', 'succeeded', $1, 'user')
		RETURNING occurred_at, clock_timestamp()`, userU).Scan(&occurred, &now); err != nil {
		t.Fatal(err)
	}
	if occurred.Year() == 2000 || occurred.After(now) {
		t.Error("a caller-supplied occurred_at was stored")
	}
}

// M2 acceptance: UPDATE and DELETE on audit rows fail, for every role and for
// the owner and superuser too.
func TestAuditEventsAreAppendOnly(t *testing.T) {
	db, admin := auditDB(t)
	id := insertCommand(t, admin)

	for _, stmt := range []string{
		`UPDATE studio.audit_events SET action = 'tampered'`,
		`DELETE FROM studio.audit_events`,
		`TRUNCATE studio.audit_events`,
	} {
		wantSQLState(t, exec(t, admin, stmt), restrictViolation, "", "superuser: "+stmt)
	}
	owner := db.Connect(t, dbroles.Migrator)
	for _, stmt := range []string{
		`SET LOCAL ROLE studio_owner; UPDATE studio.audit_events SET action = 'tampered'`,
		`SET LOCAL ROLE studio_owner; DELETE FROM studio.audit_events`,
	} {
		wantSQLState(t, exec(t, owner, stmt), restrictViolation, "", "owner: "+stmt)
	}
	for _, role := range []string{dbroles.App, dbroles.Worker, dbroles.ReadOnly, dbroles.Backup} {
		conn := db.Connect(t, role)
		for _, stmt := range []string{
			`UPDATE studio.audit_events SET action = 'tampered'`,
			`DELETE FROM studio.audit_events`,
			`TRUNCATE studio.audit_events`,
		} {
			wantSQLState(t, exec(t, conn, stmt), insufficientPriv, "", role+": "+stmt)
		}
	}
	var action string
	if err := admin.QueryRow(ctx(t), `SELECT action FROM studio.audit_events WHERE id = $1`, id).Scan(&action); err != nil || action != "thing.created" {
		t.Errorf("event changed or vanished: %q %v", action, err)
	}

	// The worker may only INSERT: it cannot read events either.
	worker := db.Connect(t, dbroles.Worker)
	wantSQLState(t, exec(t, worker, `SELECT 1 FROM studio.audit_events`), insufficientPriv, "", "worker reads audit_events")
}

// Migration 00003 records the rows seeded by 00002 (plan 8.1).
func TestSystemSeededEvent(t *testing.T) {
	db := testdb.New(t)
	admin := db.Connect(t, testdb.Admin)
	var actor, kind, method string
	var details audit.SystemSeededDetails
	var raw []byte
	if err := admin.QueryRow(ctx(t), `
		SELECT actor_principal_id::text, actor_kind::text, auth_method, details FROM studio.audit_events
		WHERE action = 'system.seeded'`).Scan(&actor, &kind, &method, &raw); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &details); err != nil {
		t.Fatal(err)
	}
	if actor != principals.RootID || kind != "system" || method != "migration" ||
		len(details.Principals) != 3 || !details.Installation || details.Migration != 2 {
		t.Errorf("system.seeded = %s %s %s %+v", actor, kind, method, details)
	}
	if n := countEvents(t, admin, `action = 'system.seeded'`); n != 1 {
		t.Errorf("%d system.seeded events, want 1", n)
	}
}

// P5: the anonymous actor constraint (plan 7.1).
func TestP5AnonymousActor(t *testing.T) {
	_, admin := auditDB(t)
	ins := `INSERT INTO studio.audit_events (action, action_class, outcome, actor_principal_id, actor_kind)
	        VALUES ($1, $2, $3, $4, $5::studio.actor_kind)`
	wantSQLState(t, exec(t, admin, ins, "auth.login_failed", "security", "failed", userU, "anonymous"),
		checkViolation, "audit_events_anonymous_actor", "anonymous with a principal")
	wantSQLState(t, exec(t, admin, ins, "thing.created", "command", "succeeded", nil, "user"),
		checkViolation, "audit_events_anonymous_actor", "non-anonymous without a principal")
	wantSQLState(t, exec(t, admin, ins, "thing.read_denied", "security", "denied", nil, "anonymous"),
		checkViolation, "audit_events_anonymous_only_login_failed", "anonymous for another action")
	wantOK(t, exec(t, admin, ins, "auth.login_failed", "security", "failed", nil, "anonymous"),
		"anonymous failed login")
}

// Plan 8.2: actor_kind must equal the referenced principal's kind.
func TestActorKindMatchesPrincipal(t *testing.T) {
	_, admin := auditDB(t)
	ins := `INSERT INTO studio.audit_events (action, action_class, outcome, actor_principal_id, actor_kind)
	        VALUES ('thing.created', 'command', 'succeeded', $1, $2::studio.actor_kind)`
	wantSQLState(t, exec(t, admin, ins, userU, "service_account"), foreignKeyViolation, "audit_events_actor_fkey", "user principal as service_account")
	wantSQLState(t, exec(t, admin, ins, userU, "system"), foreignKeyViolation, "audit_events_actor_fkey", "user principal as system")
	wantSQLState(t, exec(t, admin, ins, principals.CLIID, "user"), foreignKeyViolation, "audit_events_actor_fkey", "system principal as user")
	wantOK(t, exec(t, admin, ins, principals.CLIID, "system"), "system principal as system")
}

// Plan 7.2 (A3 per Rev 2.1 #8, definition pending in the plan): on_behalf_of
// is only allowed for system actors.
func TestOnBehalfOfOnlyForSystemActors(t *testing.T) {
	_, admin := auditDB(t)
	wantSQLState(t, exec(t, admin, `
		INSERT INTO studio.audit_events (action, action_class, outcome, actor_principal_id, actor_kind, on_behalf_of_principal_id)
		VALUES ('thing.created', 'command', 'succeeded', $1, 'user', $2)`, userU, userV),
		checkViolation, "audit_events_on_behalf_of_system_only", "user acting on behalf of another user")
	wantOK(t, exec(t, admin, `
		INSERT INTO studio.audit_events (action, action_class, outcome, actor_principal_id, actor_kind, on_behalf_of_principal_id)
		VALUES ('thing.created', 'command', 'succeeded', $1, 'system', $2)`, principals.CLIID, userU),
		"system actor on behalf of a user")
}

// JA1-JA9 (plan 17.4.1): one test per job-execution constraint of 8.2, each
// inserting a row that violates exactly that constraint.
func TestJA1ActionClassIffJobID(t *testing.T) {
	_, admin := auditDB(t)
	r := validPeriodic(1)
	r.jobID, r.attempt, r.trigger = nil, nil, nil
	wantSQLState(t, exec(t, admin, insertJobSQL, r.args()...), checkViolation, "audit_events_job_id_iff_job_execution", "job event without job_id")
	r = validPeriodic(2)
	r.action, r.class, r.actor, r.kind = "thing.created", "command", userU, "user"
	wantSQLState(t, exec(t, admin, insertJobSQL, r.args()...), checkViolation, "audit_events_job_id_iff_job_execution", "non-job event with job_id")
}

func TestJA2JobFieldsAllOrNone(t *testing.T) {
	_, admin := auditDB(t)
	for name, mut := range map[string]func(*jobRow){
		"no attempt": func(r *jobRow) { r.attempt = nil },
		"no trigger": func(r *jobRow) { r.trigger = nil },
	} {
		r := validPeriodic(3)
		mut(&r)
		wantSQLState(t, exec(t, admin, insertJobSQL, r.args()...), checkViolation, "audit_events_job_fields_together", name)
	}
}

func TestJA3JobAttemptAtLeastOne(t *testing.T) {
	_, admin := auditDB(t)
	r := validPeriodic(4)
	r.attempt = 0
	wantSQLState(t, exec(t, admin, insertJobSQL, r.args()...), checkViolation, "audit_events_job_attempt_positive", "attempt 0")
}

func TestJA4JobEventsByWorkerOnly(t *testing.T) {
	_, admin := auditDB(t)
	r := validPeriodic(5)
	r.actor, r.kind = userU, "user"
	wantSQLState(t, exec(t, admin, insertJobSQL, r.args()...), checkViolation, "audit_events_job_execution_by_worker", "job event by a user")
	r = validPeriodic(6)
	r.actor = principals.CLIID
	wantSQLState(t, exec(t, admin, insertJobSQL, r.args()...), checkViolation, "audit_events_job_execution_by_worker", "job event by system:cli")
	wantSQLState(t, exec(t, admin, `
		INSERT INTO studio.audit_events (action, action_class, outcome, actor_principal_id, actor_kind)
		VALUES ('thing.created', 'command', 'succeeded', $1, 'system')`, workerID),
		checkViolation, "audit_events_job_execution_by_worker", "non-job event by system:worker")
}

func TestJA5CommandJobNeedsCausationAndOnBehalfOf(t *testing.T) {
	_, admin := auditDB(t)
	cause := insertCommand(t, admin, 7)
	r := validCommandJob(7, cause)
	r.causation, r.causationOut = nil, nil
	wantSQLState(t, exec(t, admin, insertJobSQL, r.args()...), checkViolation, "audit_events_command_job_has_causation", "no causation")
	r = validCommandJob(7, cause)
	r.onBehalf = nil
	wantSQLState(t, exec(t, admin, insertJobSQL, r.args()...), checkViolation, "audit_events_command_job_has_causation", "no on_behalf_of")
	wantOK(t, exec(t, admin, insertJobSQL, validCommandJob(7, cause).args()...), "valid command-triggered job")
}

func TestJA6PeriodicJobHasNoCausationOrOnBehalfOf(t *testing.T) {
	_, admin := auditDB(t)
	cause := insertCommand(t, admin, 8)
	r := validPeriodic(8)
	r.onBehalf = userU
	wantSQLState(t, exec(t, admin, insertJobSQL, r.args()...), checkViolation, "audit_events_periodic_job_has_no_causation", "periodic with on_behalf_of")
	r = validPeriodic(8)
	r.causation, r.causationOut = cause, "succeeded"
	wantSQLState(t, exec(t, admin, insertJobSQL, r.args()...), checkViolation, "audit_events_periodic_job_has_no_causation", "periodic with causation")
	wantOK(t, exec(t, admin, insertJobSQL, validPeriodic(8).args()...), "valid periodic job")
}

func TestJA7CausationMustBeASucceededEvent(t *testing.T) {
	_, admin := auditDB(t)
	r := validCommandJob(9, "0190a6e4-0000-7000-8000-00000000dead")
	wantSQLState(t, exec(t, admin, insertJobSQL, r.args()...), foreignKeyViolation, "audit_events_causation_fkey", "nonexistent cause")

	var denied string
	if err := admin.QueryRow(ctx(t), `
		INSERT INTO studio.audit_events (action, action_class, outcome, actor_principal_id, actor_kind, details)
		VALUES ('thing.read_denied', 'security', 'denied', $1, 'user', '{"enqueued_jobs":[9]}') RETURNING id`, userU).Scan(&denied); err != nil {
		t.Fatal(err)
	}
	wantSQLState(t, exec(t, admin, insertJobSQL, validCommandJob(9, denied).args()...),
		foreignKeyViolation, "audit_events_causation_fkey", "denied cause")
	r = validCommandJob(9, denied)
	r.causationOut = "denied"
	wantSQLState(t, exec(t, admin, insertJobSQL, r.args()...),
		checkViolation, "audit_events_causation_outcome_succeeded", "causation_outcome denied")
}

func TestJA8CausationTrigger(t *testing.T) {
	_, admin := auditDB(t)
	notEnqueuing := insertCommand(t, admin, 99)
	wantSQLState(t, exec(t, admin, insertJobSQL, validCommandJob(10, notEnqueuing).args()...),
		checkViolation, "audit_events_causation_enqueued_job", "cause did not enqueue the job")

	cause := insertCommand(t, admin, 10)
	r := validCommandJob(10, cause)
	r.onBehalf = userV
	wantSQLState(t, exec(t, admin, insertJobSQL, r.args()...),
		checkViolation, "audit_events_causation_on_behalf_of", "on_behalf_of not inherited")

	// A cause dated after the job event, created with the server-time trigger
	// disabled inside this rolled-back transaction (superuser only).
	err := exec(t, admin, `
		ALTER TABLE studio.audit_events DISABLE TRIGGER audit_events_10_assign_server_fields;
		INSERT INTO studio.audit_events (id, occurred_at, action, action_class, outcome, actor_principal_id, actor_kind, details)
		VALUES ('0190a6e4-0000-7000-8000-00000000f00d', now() + interval '1 hour', 'thing.created', 'command', 'succeeded',
		        '`+userU+`', 'user', '{"enqueued_jobs":[11]}');
		ALTER TABLE studio.audit_events ENABLE TRIGGER audit_events_10_assign_server_fields;
		INSERT INTO studio.audit_events (action, action_class, outcome, actor_principal_id, actor_kind,
			on_behalf_of_principal_id, job_id, job_attempt, job_trigger, causation_event_id, causation_outcome)
		VALUES ('job.thing_processed', 'job_execution', 'succeeded', '`+workerID+`', 'system',
		        '`+userU+`', 11, 1, 'command', '0190a6e4-0000-7000-8000-00000000f00d', 'succeeded')`)
	wantSQLState(t, err, checkViolation, "audit_events_causation_earlier", "cause did not occur earlier")

	wantOK(t, exec(t, admin, insertJobSQL, validCommandJob(10, cause).args()...), "valid causation")
}

func TestJA9OneJobExecutionEventPerJob(t *testing.T) {
	_, admin := auditDB(t)
	if _, err := admin.Exec(ctx(t), insertJobSQL, validPeriodic(12).args()...); err != nil {
		t.Fatal(err)
	}
	wantSQLState(t, exec(t, admin, insertJobSQL, validPeriodic(12).args()...),
		uniqueViolation, "audit_events_job_execution_job_id_key", "second event for job 12")
}

// A chunked job chain written by studio_worker through the Writer: the worker
// has INSERT only, the causation trigger reads the cause as definer, and
// on_behalf_of is inherited along the chain (plan 8.2).
func TestWorkerWritesCausedJobChain(t *testing.T) {
	db, admin := auditDB(t)
	cause := insertCommand(t, admin, 20)
	worker := db.Connect(t, dbroles.Worker)
	w := audit.NewWriter(testActions)

	tx, err := worker.Begin(ctx(t))
	if err != nil {
		t.Fatal(err)
	}
	chunk1, err := w.Record(ctx(t), tx, audit.Event{
		Action: "job.thing_processed", Outcome: audit.Succeeded, ActorPrincipalID: workerID, ActorKind: audit.System,
		OnBehalfOf: userU, CausationEventID: cause, Job: &audit.Job{ID: 20, Attempt: 1, Trigger: audit.TriggerCommand},
		Details: map[string]any{"enqueued_jobs": []int64{21}},
	})
	if err != nil {
		t.Fatalf("chunk 1: %v", err)
	}
	if err := tx.Commit(ctx(t)); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Record(ctx(t), worker, audit.Event{
		Action: "job.thing_processed", Outcome: audit.Succeeded, ActorPrincipalID: workerID, ActorKind: audit.System,
		OnBehalfOf: userU, CausationEventID: chunk1, Job: &audit.Job{ID: 21, Attempt: 2, Trigger: audit.TriggerCommand},
	}); err != nil {
		t.Fatalf("chunk 2: %v", err)
	}
	if n := countEvents(t, admin, `action_class = 'job_execution'`); n != 2 {
		t.Errorf("%d job events, want 2", n)
	}
}

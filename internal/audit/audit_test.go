package audit

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/SENEBICLABS/Senebiclabs-Dataset-Studio/internal/principals"
)

var testCatalog = Catalog{
	"thing.created":       {Class: Command, DetailsVersion: 1},
	"thing.read_denied":   {Class: Security, DetailsVersion: 2},
	ActionLoginFailed:     {Class: Security, DetailsVersion: 1},
	"job.thing_processed": {Class: JobExecution, DetailsVersion: 1},
}

const userID = "0190a6e4-0000-7000-8000-000000000001"

func userEvent(action string, outcome Outcome) Event {
	return Event{Action: action, Outcome: outcome, ActorPrincipalID: userID, ActorKind: User}
}

func TestCatalogRejectsUnknownActions(t *testing.T) {
	if _, err := testCatalog.validate(userEvent("not.listed", Succeeded)); err == nil ||
		!strings.Contains(err.Error(), "not in the catalog") {
		t.Errorf("unknown action accepted: %v", err)
	}
	if _, ok := Actions[ActionSystemSeeded]; !ok {
		t.Error("system.seeded must be in the production catalog")
	}
	for name, spec := range Actions {
		if spec.DetailsVersion < 1 {
			t.Errorf("%s has no details version", name)
		}
	}
}

func TestCatalogEnforcesStructuralRules(t *testing.T) {
	worker := principals.WorkerID
	cases := map[string]Event{
		"anonymous with principal": {Action: ActionLoginFailed, Outcome: Failed, ActorPrincipalID: userID, ActorKind: Anonymous},
		"anonymous other action":   {Action: "thing.created", Outcome: Succeeded, ActorKind: Anonymous},
		"user without principal":   {Action: "thing.created", Outcome: Succeeded, ActorKind: User},
		"on_behalf_of by user":     {Action: "thing.created", Outcome: Succeeded, ActorPrincipalID: userID, ActorKind: User, OnBehalfOf: userID},
		"job without job":          {Action: "job.thing_processed", Outcome: Succeeded, ActorPrincipalID: worker, ActorKind: System},
		"job field on command":     {Action: "thing.created", Outcome: Succeeded, ActorPrincipalID: userID, ActorKind: User, Job: &Job{ID: 1, Attempt: 1, Trigger: TriggerPeriodic}},
		"job by non-worker": {Action: "job.thing_processed", Outcome: Succeeded, ActorPrincipalID: principals.CLIID, ActorKind: System,
			Job: &Job{ID: 1, Attempt: 1, Trigger: TriggerPeriodic}},
		"job attempt zero": {Action: "job.thing_processed", Outcome: Succeeded, ActorPrincipalID: worker, ActorKind: System,
			Job: &Job{ID: 1, Attempt: 0, Trigger: TriggerPeriodic}},
		"command job without causation": {Action: "job.thing_processed", Outcome: Succeeded, ActorPrincipalID: worker, ActorKind: System,
			OnBehalfOf: userID, Job: &Job{ID: 1, Attempt: 1, Trigger: TriggerCommand}},
		"periodic job with on_behalf_of": {Action: "job.thing_processed", Outcome: Succeeded, ActorPrincipalID: worker, ActorKind: System,
			OnBehalfOf: userID, Job: &Job{ID: 1, Attempt: 1, Trigger: TriggerPeriodic}},
		"worker writing a command": {Action: "thing.created", Outcome: Succeeded, ActorPrincipalID: worker, ActorKind: System},
		"bad outcome":              {Action: "thing.created", Outcome: "maybe", ActorPrincipalID: userID, ActorKind: User},
	}
	for name, ev := range cases {
		if _, err := testCatalog.validate(ev); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	ok := Event{Action: "job.thing_processed", Outcome: Succeeded, ActorPrincipalID: worker, ActorKind: System,
		OnBehalfOf: userID, CausationEventID: "0190a6e4-0000-7000-8000-0000000000ff",
		Job: &Job{ID: 7, Attempt: 1, Trigger: TriggerCommand}}
	if _, err := testCatalog.validate(ok); err != nil {
		t.Errorf("valid command-triggered job event rejected: %v", err)
	}
}

type execRecorder struct {
	sql  string
	args []any
}

func (r *execRecorder) Exec(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	r.sql, r.args = sql, args
	return pgconn.CommandTag{}, nil
}

func TestRecordWritesOnlySucceededEvents(t *testing.T) {
	w := NewWriter(testCatalog)
	for _, o := range []Outcome{Denied, Failed} {
		if _, err := w.Record(t.Context(), nil, userEvent("thing.created", o)); err == nil {
			t.Errorf("Record accepted a %s event", o)
		}
	}
}

func TestRecordEncodesDetailsAndVersion(t *testing.T) {
	w := NewWriter(testCatalog)
	rec := &execRecorder{}
	ev := userEvent("thing.created", Succeeded)
	ev.Details = map[string]any{"name": "x"}
	id, err := w.Record(t.Context(), rec, ev)
	if err != nil {
		t.Fatal(err)
	}
	if id == "" || rec.args[0] != id {
		t.Errorf("id %q not used as the row id", id)
	}
	if !strings.Contains(rec.sql, "INSERT INTO studio.audit_events") || strings.Contains(rec.sql, "RETURNING") {
		t.Errorf("unexpected SQL: %s", rec.sql)
	}
	if string(rec.args[18].([]byte)) != `{"name":"x"}` || rec.args[19] != 1 {
		t.Errorf("details/version = %s / %v", rec.args[18], rec.args[19])
	}

	ev.Details = []string{"not", "an", "object"}
	if _, err := w.Record(t.Context(), rec, ev); err == nil {
		t.Error("non-object details accepted")
	}
}

type failingDB struct{}

func (failingDB) Begin(context.Context) (pgx.Tx, error) {
	return nil, errors.New("database unavailable")
}

func TestAggregatorCountsPerKeyAndRejectsSucceeded(t *testing.T) {
	a := NewAggregator(failingDB{}, slog.New(slog.DiscardHandler), testCatalog, AggregatorConfig{})
	if err := a.Add(userEvent("thing.read_denied", Succeeded)); err == nil {
		t.Error("aggregator accepted a succeeded event")
	}
	for range 100 {
		ev := userEvent("thing.read_denied", Denied)
		ev.TargetType, ev.TargetID = "thing", "t1"
		if err := a.Add(ev); err != nil {
			t.Fatal(err)
		}
	}
	other := userEvent("thing.read_denied", Denied)
	other.TargetType, other.TargetID = "thing", "t2"
	_ = a.Add(other)
	if got := a.Pending(); got != 2 {
		t.Errorf("pending keys = %d, want 2", got)
	}
}

// AG3 (unit level): reaching MaxKeys signals an early flush.
func TestAggregatorSignalsEarlyFlushAtMaxKeys(t *testing.T) {
	a := NewAggregator(failingDB{}, slog.New(slog.DiscardHandler), testCatalog, AggregatorConfig{MaxKeys: 3})
	for i := range 2 {
		ev := userEvent("thing.read_denied", Denied)
		ev.TargetID = string(rune('a' + i))
		_ = a.Add(ev)
	}
	select {
	case <-a.early:
		t.Fatal("early flush signalled below MaxKeys")
	default:
	}
	ev := userEvent("thing.read_denied", Denied)
	ev.TargetID = "c"
	_ = a.Add(ev)
	select {
	case <-a.early:
	case <-time.After(time.Second):
		t.Fatal("no early flush signal at MaxKeys")
	}
}

// AG2 (unit level): a failed flush logs every aggregate with its full fields
// as audit_aggregate_unflushed and drops none silently.
func TestFailedFlushLogsEveryAggregate(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&buf, nil))
	a := NewAggregator(failingDB{}, log, testCatalog, AggregatorConfig{})
	for i, target := range []string{"t1", "t1", "t2"} {
		ev := userEvent("thing.read_denied", Denied)
		ev.TargetType, ev.TargetID, ev.RequestID = "thing", target, "req-"+string(rune('0'+i))
		ev.Details = map[string]any{"reason": "no permission"}
		_ = a.Add(ev)
	}
	anon := Event{Action: ActionLoginFailed, Outcome: Failed, ActorKind: Anonymous,
		Details: map[string]any{"identifier_hmac": "abc123"}}
	_ = a.Add(anon)

	err := a.Flush(t.Context())
	if err == nil {
		t.Fatal("flush against a failing database reported success")
	}
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 3 {
		t.Fatalf("logged %d records, want 3 (one per aggregate):\n%s", len(lines), buf.String())
	}
	var counts []float64
	for _, l := range lines {
		var rec map[string]any
		if err := json.Unmarshal([]byte(l), &rec); err != nil {
			t.Fatal(err)
		}
		if rec["msg"] != UnflushedLogMessage || rec["level"] != "ERROR" {
			t.Errorf("record = %v", rec)
		}
		for _, field := range []string{"action", "outcome", "actor_kind", "details", "cause", "details_version"} {
			if _, ok := rec[field]; !ok {
				t.Errorf("record missing %s: %v", field, rec)
			}
		}
		var details map[string]any
		_ = json.Unmarshal([]byte(rec["details"].(string)), &details)
		counts = append(counts, details["repeat_count"].(float64))
	}
	if counts[0] != 2 || counts[1] != 1 || counts[2] != 1 {
		t.Errorf("repeat counts = %v, want [2 1 1]", counts)
	}
	if a.Pending() != 0 {
		t.Error("aggregates remain pending after a logged failure")
	}
}

// With an already-expired context (shutdown deadline passed, M0-F4), the
// shutdown flush still runs and falls back to logging.
func TestShutdownWithExpiredContextLogsAggregates(t *testing.T) {
	var buf bytes.Buffer
	a := NewAggregator(failingDB{}, slog.New(slog.NewJSONHandler(&buf, nil)), testCatalog, AggregatorConfig{})
	_ = a.Add(userEvent("thing.read_denied", Denied))
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := a.Shutdown(ctx, false); err == nil {
		t.Error("expected an error from an expired-context shutdown")
	}
	if !strings.Contains(buf.String(), UnflushedLogMessage) {
		t.Error("pending aggregate was not logged")
	}
}

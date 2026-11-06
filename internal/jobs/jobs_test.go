package jobs

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/SENEBICLABS/Senebiclabs-Dataset-Studio/internal/audit"
)

type aArgs struct{}

func (aArgs) Kind() string { return "test.a" }

type bArgs struct{}

func (bArgs) Kind() string { return "test.b" }

type noKindArgs struct{}

func (noKindArgs) Kind() string { return "" }

func handler[A Args](context.Context, pgx.Tx, Job[A]) (Result, error) { return Result{}, nil }

func TestRegisterValidatesKinds(t *testing.T) {
	r := NewRegistry()
	if err := Register(r, handler[aArgs], KindOptions{Action: "test.a_done"}); err != nil {
		t.Fatal(err)
	}
	cases := map[string]error{
		"duplicate":        Register(r, handler[aArgs], KindOptions{Action: "test.a_done"}),
		"no kind name":     Register(r, handler[noKindArgs], KindOptions{Action: "x"}),
		"no handler":       Register[bArgs](r, nil, KindOptions{Action: "x"}),
		"no action":        Register(r, handler[bArgs], KindOptions{}),
		"negative timeout": Register(r, handler[bArgs], KindOptions{Action: "x", Timeout: -1}),
	}
	for name, err := range cases {
		if err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
	if got := r.Kinds(); !slices.Equal(got, []string{"test.a"}) {
		t.Errorf("Kinds = %v", got)
	}
}

func TestCheckActionsRequiresCatalogedJobExecution(t *testing.T) {
	r := NewRegistry()
	_ = Register(r, handler[aArgs], KindOptions{Action: "test.a_done"})
	_ = Register(r, handler[bArgs], KindOptions{Action: "test.b_cmd"})
	err := r.CheckActions(audit.Catalog{
		"test.a_done": {Class: audit.JobExecution, DetailsVersion: 1},
		"test.b_cmd":  {Class: audit.Command, DetailsVersion: 1},
	})
	if err == nil || !strings.Contains(err.Error(), "test.b") || strings.Contains(err.Error(), "kind test.a:") {
		t.Errorf("CheckActions = %v", err)
	}
	if err := r.CheckActions(audit.Catalog{}); err == nil {
		t.Error("uncataloged actions accepted")
	}
}

func TestOriginRoundTripAndTrigger(t *testing.T) {
	o := Origin{CausationEventID: "0190a6e4-0000-7000-8000-0000000000e1", OnBehalfOf: "0190a6e4-0000-7000-8000-0000000000a1"}
	meta, err := originMetadata(o)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]string
	if err := json.Unmarshal(meta, &m); err != nil || m[metaCausation] != o.CausationEventID {
		t.Fatalf("metadata = %s, %v", meta, err)
	}
	if got := originFromMetadata(meta); got != o || got.Trigger() != audit.TriggerCommand {
		t.Errorf("round trip = %+v (%s)", got, got.Trigger())
	}
	if meta, err := originMetadata(Origin{}); err != nil || meta != nil {
		t.Errorf("periodic origin metadata = %s, %v", meta, err)
	}
	if got := originFromMetadata([]byte(`{"river:rescue_count": 1}`)); got != (Origin{}) || got.Trigger() != audit.TriggerPeriodic {
		t.Errorf("periodic origin = %+v", got)
	}
	for _, half := range []Origin{{CausationEventID: o.CausationEventID}, {OnBehalfOf: o.OnBehalfOf}} {
		if _, err := originMetadata(half); err == nil {
			t.Errorf("half-set origin accepted: %+v", half)
		}
	}
}

func TestNewRunnerRejectsBadConfig(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if _, err := NewRunner(RunnerConfig{Registry: NewRegistry(), Logger: log}); err == nil {
		t.Error("runner without a pool accepted")
	}
}

func TestProductionRegistryIsCataloged(t *testing.T) {
	r, err := Production()
	if err != nil {
		t.Fatal(err)
	}
	if err := r.CheckActions(audit.Actions); err != nil {
		t.Error(err)
	}
}

func TestArgsAdapterEncodesWrappedArgs(t *testing.T) {
	type named struct {
		Name string `json:"name"`
	}
	b, err := json.Marshal(argsAdapter{wrapped{named{"x"}}})
	if err != nil || string(b) != `{"name":"x"}` {
		t.Errorf("encoded %s, %v", b, err)
	}
}

type wrapped struct{ v any }

func (wrapped) Kind() string                   { return "test.w" }
func (w wrapped) MarshalJSON() ([]byte, error) { return json.Marshal(w.v) }

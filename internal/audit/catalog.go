package audit

import (
	"errors"
	"fmt"

	"github.com/SENEBICLABS/Senebiclabs-Dataset-Studio/internal/principals"
)

// ActionSpec describes one auditable action: its class and the version of its
// details struct. Bump DetailsVersion whenever the details struct changes.
type ActionSpec struct {
	Class          Class
	DetailsVersion int
}

// Catalog maps action names to their specs. Every action written must be in
// the catalog (plan 8.1); an unknown action is an error, never a silent write.
type Catalog map[string]ActionSpec

// Actions known to the application. Each milestone adds the actions it
// introduces, together with a details struct.
var Actions = Catalog{
	// Written once by migration 00003 for the rows seeded by migration 00002
	// (plan 8.1). Listed so readers can decode it.
	ActionSystemSeeded: {Class: Command, DetailsVersion: 1},
}

// Action names.
const (
	ActionSystemSeeded = "system.seeded"
	// ActionLoginFailed is the only action an anonymous actor may have (plan
	// 7.1). It enters the catalog with authentication in M3.
	ActionLoginFailed = "auth.login_failed"
)

// SystemSeededDetails is the details struct of system.seeded, version 1.
type SystemSeededDetails struct {
	Principals   []string `json:"principals"`
	Installation bool     `json:"installation"`
	Migration    int      `json:"migration"`
}

// validate checks ev against the catalog and the plan's structural rules. The
// database enforces the same rules; checking here gives callers clear errors.
func (c Catalog) validate(ev Event) (ActionSpec, error) {
	spec, ok := c[ev.Action]
	if !ok {
		return ActionSpec{}, fmt.Errorf("audit: action %q is not in the catalog", ev.Action)
	}
	if spec.DetailsVersion < 1 {
		return ActionSpec{}, fmt.Errorf("audit: action %q has no details version", ev.Action)
	}
	var errs []error
	switch ev.Outcome {
	case Succeeded, Denied, Failed:
	default:
		errs = append(errs, fmt.Errorf("outcome %q is not valid", ev.Outcome))
	}
	switch ev.ActorKind {
	case User, ServiceAccount, System:
		if ev.ActorPrincipalID == "" {
			errs = append(errs, fmt.Errorf("actor kind %s requires an actor principal", ev.ActorKind))
		}
	case Anonymous:
		if ev.ActorPrincipalID != "" {
			errs = append(errs, errors.New("an anonymous actor has no principal"))
		}
		if ev.Action != ActionLoginFailed {
			errs = append(errs, fmt.Errorf("an anonymous actor is only allowed for %s", ActionLoginFailed))
		}
	default:
		errs = append(errs, fmt.Errorf("actor kind %q is not valid", ev.ActorKind))
	}
	if ev.OnBehalfOf != "" && ev.ActorKind != System {
		errs = append(errs, errors.New("on_behalf_of is only allowed for system actors"))
	}
	isJob := spec.Class == JobExecution
	switch {
	case isJob && ev.Job == nil:
		errs = append(errs, errors.New("a job_execution action requires a job"))
	case !isJob && ev.Job != nil:
		errs = append(errs, errors.New("only job_execution actions carry a job"))
	case isJob:
		if ev.ActorPrincipalID != principals.WorkerID {
			errs = append(errs, errors.New("job_execution events are written by system:worker"))
		}
		if ev.Job.Attempt < 1 {
			errs = append(errs, errors.New("job attempt must be at least 1"))
		}
		switch ev.Job.Trigger {
		case TriggerCommand:
			if ev.CausationEventID == "" || ev.OnBehalfOf == "" {
				errs = append(errs, errors.New("a command-triggered job requires causation and on_behalf_of"))
			}
		case TriggerPeriodic:
			if ev.CausationEventID != "" || ev.OnBehalfOf != "" {
				errs = append(errs, errors.New("a periodic job has no causation or on_behalf_of"))
			}
		default:
			errs = append(errs, fmt.Errorf("job trigger %q is not valid", ev.Job.Trigger))
		}
	}
	if !isJob && ev.ActorPrincipalID == principals.WorkerID {
		errs = append(errs, errors.New("system:worker writes only job_execution events"))
	}
	if err := errors.Join(errs...); err != nil {
		return ActionSpec{}, fmt.Errorf("audit: invalid %s event: %w", ev.Action, err)
	}
	return spec, nil
}

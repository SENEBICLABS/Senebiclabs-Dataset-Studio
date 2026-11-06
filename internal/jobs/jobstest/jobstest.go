// Package jobstest is the idempotency harness for job kinds (plan 12.1 and
// 17.3 #8: every job kind is idempotent, and a test runs each one twice).
package jobstest

import (
	"context"
	"fmt"
)

// CheckIdempotent runs the same work twice and fails if the second run
// changes the domain state the first one produced.
//
// runOnce performs the work once to completion (for a job kind: enqueue the
// kind's fixture arguments and wait until the job is completed). digest
// summarises the domain state the kind writes; it must exclude audit_events
// and River's tables, which legitimately gain a row per job.
func CheckIdempotent(ctx context.Context, runOnce func(context.Context) error, digest func(context.Context) (string, error)) error {
	if err := runOnce(ctx); err != nil {
		return fmt.Errorf("first run: %w", err)
	}
	first, err := digest(ctx)
	if err != nil {
		return fmt.Errorf("digest after first run: %w", err)
	}
	if err := runOnce(ctx); err != nil {
		return fmt.Errorf("second run: %w", err)
	}
	second, err := digest(ctx)
	if err != nil {
		return fmt.Errorf("digest after second run: %w", err)
	}
	if first != second {
		return fmt.Errorf("not idempotent: state after one run %q, after two runs %q", first, second)
	}
	return nil
}

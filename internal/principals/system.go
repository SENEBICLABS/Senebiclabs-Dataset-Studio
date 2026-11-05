// Package principals defines the well-known system principals (plan 7.1).
package principals

// Fixed ids of the seeded system principals (migration 00002). The root id is
// fixed by the plan; the other two follow the same pattern so they can be
// referenced as constants (for example by the M2 audit constraint that a
// job_execution event's actor is system:worker).
const (
	RootID   = "00000000-0000-7000-8000-000000000001"
	CLIID    = "00000000-0000-7000-8000-000000000002"
	WorkerID = "00000000-0000-7000-8000-000000000003"
)

// Handles of the seeded system principals.
const (
	RootHandle   = "system:migration"
	CLIHandle    = "system:cli"
	WorkerHandle = "system:worker"
)

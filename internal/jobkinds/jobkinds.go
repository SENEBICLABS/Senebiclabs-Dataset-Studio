// Package jobkinds assembles the job kinds studio-worker runs. It is separate
// from internal/jobs so that domain packages can depend on jobs without a
// cycle. The cross-cutting idempotency and audit-completeness tests iterate
// over Production (plan 17.3 #2 and #8).
package jobkinds

import (
	"errors"

	"github.com/SENEBICLABS/Senebiclabs-Dataset-Studio/internal/blob"
	"github.com/SENEBICLABS/Senebiclabs-Dataset-Studio/internal/jobs"
)

// Deps are the dependencies of the production job kinds.
type Deps struct {
	Blob *blob.Maintenance
}

// Production returns every production job kind: blob.sweep and blob.audit
// (M5).
func Production(d Deps) (*jobs.Registry, error) {
	if d.Blob == nil {
		return nil, errors.New("jobkinds: blob maintenance is required")
	}
	r := jobs.NewRegistry()
	if err := blob.Register(r, d.Blob); err != nil {
		return nil, err
	}
	return r, nil
}

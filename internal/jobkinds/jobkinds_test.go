package jobkinds

import (
	"path/filepath"
	"slices"
	"testing"

	"github.com/SENEBICLABS/Senebiclabs-Dataset-Studio/internal/audit"
	"github.com/SENEBICLABS/Senebiclabs-Dataset-Studio/internal/blob"
	"github.com/SENEBICLABS/Senebiclabs-Dataset-Studio/internal/blob/fsstore"
)

func TestProductionKindsAreCatalogedAndPeriodic(t *testing.T) {
	s, err := fsstore.New(filepath.Join(t.TempDir(), "blobs"))
	if err != nil {
		t.Fatal(err)
	}
	m, err := blob.NewMaintenance(blob.MaintenanceConfig{Store: s})
	if err != nil {
		t.Fatal(err)
	}
	r, err := Production(Deps{Blob: m})
	if err != nil {
		t.Fatal(err)
	}
	if got := r.Kinds(); !slices.Equal(got, []string{blob.KindAudit, blob.KindSweep}) {
		t.Errorf("kinds = %v", got)
	}
	if err := r.CheckActions(audit.Actions); err != nil {
		t.Error(err)
	}
	for _, k := range r.Kinds() {
		if !r.Periodic(k) {
			t.Errorf("%s is not periodic", k)
		}
	}
	if _, err := Production(Deps{}); err == nil {
		t.Error("missing dependencies accepted")
	}
}

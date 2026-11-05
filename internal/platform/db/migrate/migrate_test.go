package migrate

import (
	"io/fs"
	"strings"
	"testing"

	"github.com/SENEBICLABS/Senebiclabs-Dataset-Studio/migrations"
)

func TestEmbeddedMigrationsAreForwardOnlyAndOrdered(t *testing.T) {
	names, err := fs.Glob(migrations.FS, "*.sql")
	if err != nil || len(names) == 0 {
		t.Fatalf("no migrations embedded: %v", err)
	}
	for i, n := range names {
		want := []string{"00001_", "00002_"}
		if i < len(want) && !strings.HasPrefix(n, want[i]) {
			t.Errorf("migration %d is %s, want prefix %s", i, n, want[i])
		}
		body, err := fs.ReadFile(migrations.FS, n)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(body), "-- +goose Up") {
			t.Errorf("%s has no Up section", n)
		}
		if strings.Contains(string(body), "-- +goose Down") {
			t.Errorf("%s has a Down section; migrations are forward-only", n)
		}
	}
	v, err := LatestVersion()
	if err != nil {
		t.Fatal(err)
	}
	if v != int64(len(names)) {
		t.Errorf("latest version %d, but %d migrations are embedded (versions must be contiguous)", v, len(names))
	}
}

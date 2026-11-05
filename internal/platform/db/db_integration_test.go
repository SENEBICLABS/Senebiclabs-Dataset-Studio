//go:build integration

package db_test

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/SENEBICLABS/Senebiclabs-Dataset-Studio/internal/platform/db"
)

// testDatabaseURL is required under the integration tag: a missing variable
// fails the run instead of silently skipping it.
func testDatabaseURL(t *testing.T) string {
	t.Helper()
	u := os.Getenv("STUDIO_TEST_DATABASE_URL")
	if u == "" {
		t.Fatal("STUDIO_TEST_DATABASE_URL is not set; run `make up` and `make test-integration`")
	}
	return u
}

func TestPingCheckerAgainstComposePostgres(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	pool, err := db.Open(ctx, testDatabaseURL(t), "studio-m0-test")
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	if err := (db.PingChecker{Pool: pool}).Check(ctx); err != nil {
		t.Fatalf("ping: %v", err)
	}

	var major int
	if err := pool.QueryRow(ctx, "SELECT current_setting('server_version_num')::int / 10000").Scan(&major); err != nil {
		t.Fatal(err)
	}
	if major != 18 {
		t.Errorf("server major version = %d, want 18 (compose pin)", major)
	}
}

func TestPingCheckerFailsWhenServerUnreachable(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// Port 1 on loopback: connection refused.
	pool, err := db.Open(ctx, "postgres://nobody:x@127.0.0.1:1/none?sslmode=disable&connect_timeout=2", "studio-m0-test")
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err := (db.PingChecker{Pool: pool}).Check(ctx); err == nil {
		t.Fatal("expected ping failure")
	}
}

func TestOpenRejectsMalformedURLWithoutEchoingIt(t *testing.T) {
	_, err := db.Open(context.Background(), "postgres://u:leakme@[::1", "x")
	if err == nil {
		t.Fatal("expected error")
	}
	if got := err.Error(); strings.Contains(got, "leakme") {
		t.Errorf("error leaks password: %s", got)
	}
}

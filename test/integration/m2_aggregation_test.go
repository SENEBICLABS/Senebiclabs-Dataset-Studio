//go:build integration

package integration

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	oscmd "os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/SENEBICLABS/Senebiclabs-Dataset-Studio/internal/app"
	"github.com/SENEBICLABS/Senebiclabs-Dataset-Studio/internal/audit"
	"github.com/SENEBICLABS/Senebiclabs-Dataset-Studio/internal/dbroles"
)

func deniedBy(actor, target string) audit.Event {
	return audit.Event{Action: "thing.read_denied", Outcome: audit.Denied, ActorPrincipalID: actor, ActorKind: audit.User,
		TargetType: "thing", TargetID: target, Details: map[string]any{"reason": "no permission"}}
}

// A5 (Rev 2.1 #8, definition pending in the plan): 100 denied requests within
// one window produce one aggregated event with repeat_count 100, written by
// INSERT only.
func TestAggregatesDeniedEventsIntoOneRow(t *testing.T) {
	db, admin := auditDB(t)
	agg := audit.NewAggregator(appPool(t, db), slog.New(slog.DiscardHandler), testActions, audit.AggregatorConfig{})
	for range 100 {
		if err := agg.Add(deniedBy(userU, "t1")); err != nil {
			t.Fatal(err)
		}
	}
	if err := agg.Add(deniedBy(userV, "t1")); err != nil {
		t.Fatal(err)
	}
	if err := agg.Flush(ctx(t)); err != nil {
		t.Fatal(err)
	}
	var count, rows int
	var raw []byte
	if err := admin.QueryRow(ctx(t), `
		SELECT (details->>'repeat_count')::int, details, count(*) OVER ()
		FROM studio.audit_events WHERE action = 'thing.read_denied' AND actor_principal_id = $1`, userU).
		Scan(&count, &raw, &rows); err != nil {
		t.Fatal(err)
	}
	if rows != 1 || count != 100 {
		t.Errorf("user U: %d rows, repeat_count %d; want 1 row with 100", rows, count)
	}
	if !strings.Contains(string(raw), `"reason": "no permission"`) || !strings.Contains(string(raw), "first_seen") {
		t.Errorf("aggregated details lost fields: %s", raw)
	}
	if n := countEvents(t, admin, `action = 'thing.read_denied'`); n != 2 {
		t.Errorf("%d aggregated rows, want 2 (one per actor)", n)
	}
}

// AG1: on graceful shutdown the flush runs as a hook after HTTP has drained,
// and writes every pending aggregate, including those produced by in-flight
// requests.
func TestAG1ShutdownFlushAfterHTTPDrain(t *testing.T) {
	db, admin := auditDB(t)
	pool := appPool(t, db)
	agg := audit.NewAggregator(pool, slog.New(slog.DiscardHandler), testActions, audit.AggregatorConfig{Window: time.Hour})
	go agg.Run()

	started, release := make(chan struct{}), make(chan struct{})
	srv := &http.Server{ReadHeaderTimeout: time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = agg.Add(deniedBy(userU, "before-drain"))
		close(started)
		<-release
		_ = agg.Add(deniedBy(userU, "during-drain")) // produced while HTTP is draining
		w.WriteHeader(http.StatusForbidden)
	})}
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx(t), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	life := app.New(slog.New(slog.DiscardHandler), 10*time.Second)
	life.ServeHTTP(srv, ln)
	flushedAfterDrain := make(chan bool, 1)
	life.OnShutdown("audit aggregator flush", func(c context.Context) error {
		flushedAfterDrain <- agg.Pending() == 2 // both in-flight events are visible to the hook
		return agg.Shutdown(c, true)
	})

	runCtx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- life.Run(runCtx) }()
	go func() {
		req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://"+ln.Addr().String()+"/", nil)
		if resp, err := http.DefaultClient.Do(req); err == nil {
			_ = resp.Body.Close()
		}
	}()
	<-started
	cancel()
	time.Sleep(50 * time.Millisecond)
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("lifecycle: %v", err)
	}
	if !<-flushedAfterDrain {
		t.Error("the flush hook ran before the in-flight request finished")
	}
	if n := countEvents(t, admin, `action = 'thing.read_denied' AND target_id IN ('before-drain', 'during-drain')`); n != 2 {
		t.Errorf("%d aggregates flushed on shutdown, want 2", n)
	}
}

// AG2: when the shutdown flush fails (database unavailable), every pending
// aggregate is logged as audit_aggregate_unflushed with its full fields.
func TestAG2FailedShutdownFlushIsLogged(t *testing.T) {
	db, admin := auditDB(t)
	pool := appPool(t, db)
	var logs bytes.Buffer
	agg := audit.NewAggregator(pool, slog.New(slog.NewJSONHandler(&logs, nil)), testActions, audit.AggregatorConfig{Window: time.Hour})
	_ = agg.Add(deniedBy(userU, "a"))
	_ = agg.Add(deniedBy(userU, "a"))
	_ = agg.Add(deniedBy(userV, "b"))
	pool.Close() // the database becomes unavailable to the aggregator

	if err := agg.Shutdown(ctx(t), false); err == nil {
		t.Fatal("shutdown flush against a closed pool reported success")
	}
	var unflushed []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		var rec map[string]any
		if json.Unmarshal([]byte(line), &rec) == nil && rec["msg"] == audit.UnflushedLogMessage {
			unflushed = append(unflushed, rec)
		}
	}
	if len(unflushed) != 2 {
		t.Fatalf("%d unflushed records logged, want 2:\n%s", len(unflushed), logs.String())
	}
	for _, rec := range unflushed {
		if rec["level"] != "ERROR" || rec["action"] != "thing.read_denied" || rec["actor_principal_id"] == "" ||
			rec["target_id"] == "" || !strings.Contains(rec["details"].(string), "repeat_count") {
			t.Errorf("incomplete unflushed record: %v", rec)
		}
	}
	if n := countEvents(t, admin, `action = 'thing.read_denied'`); n != 0 {
		t.Errorf("%d rows written despite the failure", n)
	}
}

// AG3: reaching 10,000 distinct keys triggers an early flush before the
// 60-second window closes.
func TestAG3EarlyFlushAtTenThousandKeys(t *testing.T) {
	db, admin := auditDB(t)
	agg := audit.NewAggregator(appPool(t, db), slog.New(slog.DiscardHandler), testActions, audit.AggregatorConfig{})
	go agg.Run()
	t.Cleanup(func() { _ = agg.Shutdown(context.Background(), true) })

	start := time.Now()
	for i := range audit.DefaultMaxKeys {
		if err := agg.Add(deniedBy(userU, fmt.Sprintf("t%05d", i))); err != nil {
			t.Fatal(err)
		}
	}
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if countEvents(t, admin, `action = 'thing.read_denied'`) == audit.DefaultMaxKeys {
			if elapsed := time.Since(start); elapsed >= audit.DefaultWindow {
				t.Fatalf("flush took %s; it must happen before the %s window closes", elapsed, audit.DefaultWindow)
			}
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("10,000 keys were not flushed early; %d rows", countEvents(t, admin, `action = 'thing.read_denied'`))
}

// AG4 helper: runs in a child process (see TestAG4HardCrashLossBounded).
func TestAG4HelperProcess(t *testing.T) {
	url := os.Getenv("STUDIO_AG4_HELPER_URL")
	if url == "" {
		return // not running as the helper
	}
	c := context.Background()
	pool, err := pgxpool.New(c, url)
	if err != nil {
		fmt.Println("ERROR", err)
		os.Exit(1)
	}
	agg := audit.NewAggregator(pool, slog.New(slog.DiscardHandler), testActions, audit.AggregatorConfig{Window: time.Hour})
	for range 3 {
		_ = agg.Add(deniedBy(userU, "window-1"))
	}
	if err := agg.Flush(c); err != nil { // window 1 closes
		fmt.Println("ERROR", err)
		os.Exit(1)
	}
	tx, err := pool.Begin(c)
	if err == nil {
		_, err = audit.NewWriter(testActions).Record(c, tx, audit.Event{Action: "thing.created", Outcome: audit.Succeeded,
			ActorPrincipalID: userU, ActorKind: audit.User, TargetType: "thing", TargetID: "transactional"})
		if err == nil {
			err = tx.Commit(c)
		}
	}
	if err != nil {
		fmt.Println("ERROR", err)
		os.Exit(1)
	}
	for range 2 {
		_ = agg.Add(deniedBy(userU, "window-2"))
	}
	fmt.Println("READY")
	select {} // wait to be killed
}

// AG4: after a hard crash (SIGKILL), loss is bounded to the current window;
// earlier windows and succeeded (transactional) events survive.
func TestAG4HardCrashLossBounded(t *testing.T) {
	db, admin := auditDB(t)
	cmd := oscmd.Command(os.Args[0], "-test.run=^TestAG4HelperProcess$", "-test.count=1") //nolint:noctx,gosec // re-executes this test binary itself with fixed arguments (helper-process pattern); killed explicitly below
	cmd.Env = append(os.Environ(), "STUDIO_AG4_HELPER_URL="+db.URL(dbroles.App))
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	ready := make(chan string, 1)
	go func() {
		sc := bufio.NewScanner(stdout)
		for sc.Scan() {
			if line := sc.Text(); line == "READY" || strings.HasPrefix(line, "ERROR") {
				ready <- line
				return
			}
		}
		ready <- "EOF"
	}()
	select {
	case line := <-ready:
		if line != "READY" {
			_ = cmd.Process.Kill()
			t.Fatalf("helper failed: %s", line)
		}
	case <-time.After(60 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("helper never became ready")
	}
	if err := cmd.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()

	var window1 int
	if err := admin.QueryRow(ctx(t), `SELECT (details->>'repeat_count')::int FROM studio.audit_events
		WHERE action = 'thing.read_denied' AND target_id = 'window-1'`).Scan(&window1); err != nil || window1 != 3 {
		t.Errorf("window 1 aggregate: repeat_count %d (%v), want 3", window1, err)
	}
	if n := countEvents(t, admin, `target_id = 'window-2'`); n != 0 {
		t.Errorf("window 2 should be lost on SIGKILL, found %d rows", n)
	}
	if n := countEvents(t, admin, `action = 'thing.created' AND target_id = 'transactional'`); n != 1 {
		t.Errorf("succeeded transactional event missing after crash: %d", n)
	}
}

//go:build integration

// Package e2e runs the built binaries as real processes. It requires
// STUDIO_BIN_DIR (set by `make test-integration`, which builds first) and the
// internal/testdb environment. Since M1 the binaries connect as their own
// login roles (studio_app, studio_worker) to a cloned test database; the
// superuser is used only by the harness for setup and inspection.
package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/SENEBICLABS/Senebiclabs-Dataset-Studio/internal/dbroles"
	"github.com/SENEBICLABS/Senebiclabs-Dataset-Studio/internal/testdb"
)

func TestMain(m *testing.M) { os.Exit(testdb.Main(m)) }

// e2eHMACKey is a test-only key for STUDIO_AUDIT_HMAC_KEY, which the binaries
// require whenever a database is configured.
const e2eHMACKey = "e2e-only-audit-hmac-key-0123456789abcdef"

func requireEnv(t *testing.T, key string) string {
	t.Helper()
	v := os.Getenv(key)
	if v == "" {
		t.Fatalf("%s is not set; run `make up` then `make test-integration`", key)
	}
	return v
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

type proc struct {
	cmd  *exec.Cmd
	logs *syncBuffer
	// done receives cmd.Wait's result exactly once. Only the test goroutine
	// reads it, and it sets exited after doing so, so Cleanup never needs to
	// read cmd.ProcessState while the Wait goroutine could be writing it.
	done   chan error
	exited bool
}

func start(t *testing.T, binary string, env ...string) *proc {
	t.Helper()
	bin := filepath.Join(requireEnv(t, "STUDIO_BIN_DIR"), binary)
	cmd := exec.Command(bin) //nolint:noctx // lifetime is managed explicitly with signals
	// Hermetic environment: only PATH plus what the test passes.
	cmd.Env = append([]string{"PATH=" + os.Getenv("PATH")}, env...)
	logs := &syncBuffer{}
	cmd.Stdout, cmd.Stderr = logs, logs
	if err := cmd.Start(); err != nil {
		t.Fatalf("start %s: %v", binary, err)
	}
	p := &proc{cmd: cmd, logs: logs, done: make(chan error, 1)}
	go func() { p.done <- cmd.Wait() }()
	t.Cleanup(func() {
		if !p.exited {
			// The test ended without observing exit (it failed or never
			// stopped the process). Kill it and reap it. Kill fails only if
			// the process has already exited. Wait then returns nil or an
			// *exec.ExitError for the kill signal, both expected here; any
			// other error means reaping failed and is reported.
			_ = cmd.Process.Kill()
			var exitErr *exec.ExitError
			if err := <-p.done; err != nil && !errors.As(err, &exitErr) {
				t.Logf("reaping %s: %v", binary, err)
			}
		}
		if t.Failed() {
			t.Logf("%s logs:\n%s", binary, logs.String())
		}
	})
	return p
}

// stop sends SIGTERM and returns the exit code.
func (p *proc) stop(t *testing.T, within time.Duration) int {
	t.Helper()
	if err := p.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("signal: %v", err)
	}
	return p.wait(t, within)
}

func (p *proc) wait(t *testing.T, within time.Duration) int {
	t.Helper()
	select {
	case err := <-p.done:
		p.exited = true
		// A non-zero exit is reported as *exec.ExitError; the exit code is
		// what callers assert on. Any other error means Wait itself failed
		// (for example copying output), and the result cannot be trusted.
		var exitErr *exec.ExitError
		if err != nil && !errors.As(err, &exitErr) {
			t.Fatalf("waiting for %s: %v", p.cmd.Path, err)
		}
		return p.cmd.ProcessState.ExitCode()
	case <-time.After(within):
		t.Fatalf("process did not exit within %s", within)
		return -1
	}
}

func (p *proc) waitForLog(t *testing.T, substr string, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if strings.Contains(p.logs.String(), substr) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("log line %q not seen within %s", substr, within)
}

func freeAddr(t *testing.T) string {
	t.Helper()
	var lc net.ListenConfig
	ln, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

func httpGet(t *testing.T, url string) (int, http.Header, []byte) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, nil, nil
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header, body
}

func waitHealthy(t *testing.T, base string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if code, _, _ := httpGet(t, base+"/healthz"); code == http.StatusOK {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("API never became healthy")
}

func TestAPIServesHealthReadinessAndShutsDownGracefully(t *testing.T) {
	db := testdb.New(t)
	addr := freeAddr(t)
	base := "http://" + addr
	p := start(t, "studio-api",
		"STUDIO_HTTP_ADDR="+addr,
		"STUDIO_DATABASE_URL="+db.URL(dbroles.App),
		"STUDIO_AUDIT_HMAC_KEY="+e2eHMACKey,
		"STUDIO_SHUTDOWN_TIMEOUT=10s",
	)
	waitHealthy(t, base)

	code, _, body := httpGet(t, base+"/readyz")
	var rep struct {
		Status string                    `json:"status"`
		Checks map[string]map[string]any `json:"checks"`
	}
	if err := json.Unmarshal(body, &rep); err != nil {
		t.Fatalf("readyz body: %v: %s", err, body)
	}
	if code != http.StatusOK || rep.Status != "ready" || rep.Checks["database"]["status"] != "ok" {
		t.Fatalf("readyz = %d %s", code, body)
	}
	assertConnectedAs(t, db, "studio-api", dbroles.App)

	code, hdr, body := httpGet(t, base+"/no-such-route")
	if code != http.StatusNotFound || hdr.Get("Content-Type") != "application/problem+json" || hdr.Get("X-Request-ID") == "" {
		t.Errorf("404 = %d %q %q %s", code, hdr.Get("Content-Type"), hdr.Get("X-Request-ID"), body)
	}

	if exit := p.stop(t, 15*time.Second); exit != 0 {
		t.Fatalf("exit code %d", exit)
	}
	logs := p.logs.String()
	for _, want := range []string{`"msg":"starting"`, `"msg":"shutdown requested"`, `"msg":"shutdown complete"`} {
		if !strings.Contains(logs, want) {
			t.Errorf("logs missing %s", want)
		}
	}
	// Every line must be JSON; the configured password must never appear.
	for _, line := range strings.Split(strings.TrimSpace(logs), "\n") {
		if !json.Valid([]byte(line)) {
			t.Errorf("non-JSON log line: %s", line)
		}
	}
	if pw := testdb.Passwords(t).App; strings.Contains(logs, pw) {
		t.Error("logs contain the database password")
	}
}

func TestAPINotReadyWhenDatabaseUnreachable(t *testing.T) {
	addr := freeAddr(t)
	base := "http://" + addr
	p := start(t, "studio-api",
		"STUDIO_HTTP_ADDR="+addr,
		"STUDIO_DATABASE_URL=postgres://nobody:x@127.0.0.1:1/none?sslmode=disable&connect_timeout=1",
		"STUDIO_AUDIT_HMAC_KEY="+e2eHMACKey,
		"STUDIO_READINESS_TIMEOUT=1s",
	)
	waitHealthy(t, base)
	if code, _, body := httpGet(t, base+"/readyz"); code != http.StatusServiceUnavailable {
		t.Errorf("readyz = %d %s", code, body)
	}
	if exit := p.stop(t, 15*time.Second); exit != 0 {
		t.Errorf("exit code %d", exit)
	}
}

func TestAPIRejectsInvalidConfiguration(t *testing.T) {
	p := start(t, "studio-api", "STUDIO_LOG_LEVEL=loud", "STUDIO_HTTP_ADDR=nope")
	if exit := p.wait(t, 5*time.Second); exit != 2 {
		t.Errorf("exit code %d, want 2", exit)
	}
	for _, want := range []string{"STUDIO_LOG_LEVEL", "STUDIO_HTTP_ADDR"} {
		if !strings.Contains(p.logs.String(), want) {
			t.Errorf("error output does not mention %s", want)
		}
	}
}

func TestWorkerStartsAndShutsDownGracefully(t *testing.T) {
	db := testdb.New(t)
	p := start(t, "studio-worker",
		"STUDIO_DATABASE_URL="+db.URL(dbroles.Worker),
		"STUDIO_AUDIT_HMAC_KEY="+e2eHMACKey)
	p.waitForLog(t, "worker idle", 10*time.Second)
	if exit := p.stop(t, 15*time.Second); exit != 0 {
		t.Fatalf("exit code %d", exit)
	}
	if !strings.Contains(p.logs.String(), `"msg":"shutdown complete"`) {
		t.Error("worker did not log shutdown complete")
	}
}

func TestStudioctlVersion(t *testing.T) {
	bin := filepath.Join(requireEnv(t, "STUDIO_BIN_DIR"), "studioctl")
	out, err := exec.Command(bin, "version").Output() //nolint:noctx // short-lived
	if err != nil {
		t.Fatal(err)
	}
	var v map[string]string
	if err := json.Unmarshal(out, &v); err != nil {
		t.Fatalf("not JSON: %s", out)
	}
	if v["module"] != "github.com/SENEBICLABS/Senebiclabs-Dataset-Studio" {
		t.Errorf("module = %q", v["module"])
	}
	if want := os.Getenv("STUDIO_EXPECT_COMMIT"); want != "" && v["commit"] != want {
		t.Errorf("commit = %q, want %q (ldflags injection)", v["commit"], want)
	}
}

// assertConnectedAs proves the process logged in as its own role, not the
// superuser, by inspecting pg_stat_activity as the harness superuser.
func assertConnectedAs(t *testing.T, db *testdb.DB, application, role string) {
	t.Helper()
	admin := db.Connect(t, testdb.Admin)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	rows, err := admin.Query(ctx, `SELECT DISTINCT usename::text FROM pg_stat_activity
		WHERE datname = current_database() AND application_name = $1`, application)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var users []string
	for rows.Next() {
		var u string
		if err := rows.Scan(&u); err != nil {
			t.Fatal(err)
		}
		users = append(users, u)
	}
	if len(users) != 1 || users[0] != role {
		t.Errorf("%s connected as %v, want only %s", application, users, role)
	}
}

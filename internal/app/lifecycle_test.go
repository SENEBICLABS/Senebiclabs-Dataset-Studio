package app

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestHooksRunInReverseOrderAfterDrain(t *testing.T) {
	var (
		mu    sync.Mutex
		order []string
	)
	rec := func(s string) { mu.Lock(); order = append(order, s); mu.Unlock() }

	lc := New(quiet(), time.Second)
	lc.OnDrain(func() { rec("drain") })
	lc.OnShutdown("first", func(context.Context) error { rec("first"); return nil })
	lc.OnShutdown("second", func(context.Context) error { rec("second"); return nil })

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := lc.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := strings.Join(order, ","); got != "drain,second,first" {
		t.Errorf("order = %s", got)
	}
}

func TestHookErrorsAreReturnedAndLaterHooksStillRun(t *testing.T) {
	ran := false
	lc := New(quiet(), time.Second)
	lc.OnShutdown("runs-last", func(context.Context) error { ran = true; return nil })
	lc.OnShutdown("fails", func(context.Context) error { return errors.New("flush failed") })

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := lc.Run(ctx)
	if err == nil || !strings.Contains(err.Error(), "flush failed") {
		t.Errorf("err = %v", err)
	}
	if !ran {
		t.Error("hook after a failing hook did not run")
	}
}

func TestDeadlineSharedAcrossHooks(t *testing.T) {
	lc := New(quiet(), 50*time.Millisecond)
	skipped := true
	lc.OnShutdown("never", func(context.Context) error { skipped = false; return nil })
	lc.OnShutdown("slow", func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() })

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	err := lc.Run(ctx)
	if time.Since(start) > time.Second {
		t.Error("shutdown exceeded deadline")
	}
	if err == nil || !strings.Contains(err.Error(), "hook never skipped") {
		t.Errorf("err = %v", err)
	}
	if !skipped {
		t.Error("hook ran after deadline expired")
	}
}

func TestInFlightRequestDrainsBeforeHooks(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var (
		mu     sync.Mutex
		events []string
	)
	rec := func(s string) { mu.Lock(); events = append(events, s); mu.Unlock() }

	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			close(started)
			<-release
			rec("request-finished")
			w.WriteHeader(http.StatusNoContent)
		}),
		ReadHeaderTimeout: time.Second,
	}
	var lcfg net.ListenConfig
	ln, err := lcfg.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	lc := New(quiet(), 5*time.Second)
	lc.ServeHTTP(srv, ln)
	lc.OnShutdown("hook", func(context.Context) error { rec("hook"); return nil })

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- lc.Run(ctx) }()

	respCh := make(chan int, 1)
	go func() {
		req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://"+ln.Addr().String()+"/", nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			respCh <- -1
			return
		}
		_ = resp.Body.Close()
		respCh <- resp.StatusCode
	}()

	<-started
	cancel() // shutdown begins while the request is in flight
	time.Sleep(50 * time.Millisecond)
	close(release)

	if code := <-respCh; code != http.StatusNoContent {
		t.Errorf("in-flight request got %d; it must complete during drain", code)
	}
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if strings.Join(events, ",") != "request-finished,hook" {
		t.Errorf("events = %v; hooks must run after HTTP drains", events)
	}
}

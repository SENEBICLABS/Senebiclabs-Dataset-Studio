package audit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Aggregation parameters (plan 8.3).
const (
	DefaultWindow       = 60 * time.Second
	DefaultMaxKeys      = 10000
	MaxShutdownFlush    = 5 * time.Second
	UnflushedLogMessage = "audit_aggregate_unflushed"
)

// Beginner starts a transaction; *pgxpool.Pool and *pgx.Conn satisfy it.
type Beginner interface {
	Begin(ctx context.Context) (pgx.Tx, error)
}

// AggregatorConfig configures an Aggregator. Zero values take the plan's
// defaults.
type AggregatorConfig struct {
	Window  time.Duration
	MaxKeys int
	Now     func() time.Time
}

type aggKey struct {
	actor, action, targetType, targetID string
	outcome                             Outcome
}

type aggregate struct {
	first     Event
	spec      ActionSpec
	count     int
	firstSeen time.Time
	lastSeen  time.Time
}

// Aggregator collects denied and failed events and writes one event per key
// per window (plan 8.3). It is best-effort across hard crashes: a SIGKILL or
// OOM loses at most the current window. Succeeded events never pass through
// it; they are written transactionally by Writer.
type Aggregator struct {
	db      Beginner
	log     *slog.Logger
	catalog Catalog
	window  time.Duration
	maxKeys int
	now     func() time.Time

	mu      sync.Mutex
	pending map[aggKey]*aggregate
	order   []aggKey

	flushMu  sync.Mutex // serialises flushes
	early    chan struct{}
	stop     chan struct{}
	done     chan struct{}
	stopOnce sync.Once
}

// NewAggregator returns an Aggregator writing through db.
func NewAggregator(db Beginner, log *slog.Logger, catalog Catalog, cfg AggregatorConfig) *Aggregator {
	if cfg.Window <= 0 {
		cfg.Window = DefaultWindow
	}
	if cfg.MaxKeys <= 0 {
		cfg.MaxKeys = DefaultMaxKeys
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Aggregator{
		db: db, log: log, catalog: catalog,
		window: cfg.Window, maxKeys: cfg.MaxKeys, now: cfg.Now,
		pending: map[aggKey]*aggregate{},
		early:   make(chan struct{}, 1),
		stop:    make(chan struct{}),
		done:    make(chan struct{}),
	}
}

func actorKey(ev Event) string {
	if ev.ActorPrincipalID != "" {
		return ev.ActorPrincipalID
	}
	// Anonymous actors (failed logins) are keyed by the identifier HMAC the
	// caller puts in details (plan 8.1); never by the raw identifier.
	if m, ok := ev.Details.(map[string]any); ok {
		if h, ok := m["identifier_hmac"].(string); ok {
			return "hmac:" + h
		}
	}
	return "anonymous"
}

// Add records a denied or failed event for aggregation. It never blocks on
// the database. Reaching MaxKeys distinct keys triggers an early flush.
func (a *Aggregator) Add(ev Event) error {
	if ev.Outcome != Denied && ev.Outcome != Failed {
		return fmt.Errorf("audit: the aggregator takes denied and failed events only, got %s", ev.Outcome)
	}
	spec, err := a.catalog.validate(ev)
	if err != nil {
		return err
	}
	if spec.Class == JobExecution {
		return errors.New("audit: job_execution events are not aggregated")
	}
	k := aggKey{actor: actorKey(ev), action: ev.Action, targetType: ev.TargetType, targetID: ev.TargetID, outcome: ev.Outcome}
	now := a.now()

	a.mu.Lock()
	agg, ok := a.pending[k]
	if ok {
		agg.count++
		agg.lastSeen = now
	} else {
		a.pending[k] = &aggregate{first: ev, spec: spec, count: 1, firstSeen: now, lastSeen: now}
		a.order = append(a.order, k)
	}
	full := len(a.pending) >= a.maxKeys
	a.mu.Unlock()

	if full {
		select {
		case a.early <- struct{}{}:
		default:
		}
	}
	return nil
}

// Pending reports how many distinct keys await flushing.
func (a *Aggregator) Pending() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.pending)
}

// Run flushes at every window close and on early-flush signals, until
// Shutdown. Call it in its own goroutine.
func (a *Aggregator) Run() {
	defer close(a.done)
	ticker := time.NewTicker(a.window)
	defer ticker.Stop()
	for {
		select {
		case <-a.stop:
			return
		case <-ticker.C:
		case <-a.early:
		}
		ctx, cancel := context.WithTimeout(context.Background(), a.window)
		_ = a.Flush(ctx) // failures are logged inside Flush
		cancel()
	}
}

// Shutdown stops Run (if started) and performs the final flush within at most
// MaxShutdownFlush of ctx (plan 8.3). With an already-expired ctx the flush
// fails immediately and every pending aggregate is logged instead.
func (a *Aggregator) Shutdown(ctx context.Context, runStarted bool) error {
	a.stopOnce.Do(func() { close(a.stop) })
	if runStarted {
		select {
		case <-a.done:
		case <-ctx.Done():
		}
	}
	fctx, cancel := context.WithTimeout(ctx, MaxShutdownFlush)
	defer cancel()
	return a.Flush(fctx)
}

// Flush writes every pending aggregate in one transaction. On failure each
// aggregate is logged at ERROR as audit_aggregate_unflushed with its full
// fields, so none is silently dropped, and the error is returned.
func (a *Aggregator) Flush(ctx context.Context) error {
	a.flushMu.Lock()
	defer a.flushMu.Unlock()

	a.mu.Lock()
	batch := make([]*aggregate, 0, len(a.order))
	for _, k := range a.order {
		batch = append(batch, a.pending[k])
	}
	a.pending = map[aggKey]*aggregate{}
	a.order = nil
	a.mu.Unlock()

	if len(batch) == 0 {
		return nil
	}
	err := a.write(ctx, batch)
	if err == nil {
		return nil
	}
	for _, agg := range batch {
		a.logUnflushed(agg, err)
	}
	return fmt.Errorf("audit: flush %d aggregates: %w", len(batch), err)
}

func aggregatedDetails(agg *aggregate) ([]byte, error) {
	base := map[string]any{}
	if agg.first.Details != nil {
		raw, err := encodeDetails(agg.first.Details)
		if err != nil {
			return nil, err
		}
		if err := json.Unmarshal(raw, &base); err != nil {
			return nil, fmt.Errorf("audit: decode details: %w", err)
		}
	}
	base["repeat_count"] = agg.count
	base["first_seen"] = agg.firstSeen.UTC().Format(time.RFC3339Nano)
	base["last_seen"] = agg.lastSeen.UTC().Format(time.RFC3339Nano)
	return json.Marshal(base)
}

func (a *Aggregator) write(ctx context.Context, batch []*aggregate) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	tx, err := a.db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	for _, agg := range batch {
		id, err := uuid.NewV7()
		if err != nil {
			return fmt.Errorf("generate id: %w", err)
		}
		details, err := aggregatedDetails(agg)
		if err != nil {
			return err
		}
		if err := insert(ctx, tx, id.String(), agg.spec, agg.first, details); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func (a *Aggregator) logUnflushed(agg *aggregate, cause error) {
	details, err := aggregatedDetails(agg)
	if err != nil {
		details = []byte(`{"error":"details could not be encoded"}`)
	}
	ev := agg.first
	a.log.Error(UnflushedLogMessage,
		slog.String("action", ev.Action),
		slog.String("action_class", string(agg.spec.Class)),
		slog.String("outcome", string(ev.Outcome)),
		slog.String("actor_principal_id", ev.ActorPrincipalID),
		slog.String("actor_kind", string(ev.ActorKind)),
		slog.String("auth_method", ev.AuthMethod),
		slog.String("target_type", ev.TargetType),
		slog.String("target_id", ev.TargetID),
		slog.String("project_id", ev.ProjectID),
		slog.String("dataset_id", ev.DatasetID),
		slog.String("request_id", ev.RequestID),
		slog.Int("details_version", agg.spec.DetailsVersion),
		slog.String("details", string(details)),
		slog.String("cause", cause.Error()),
	)
}

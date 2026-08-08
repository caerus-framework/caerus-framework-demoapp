// Package catalogsummary owns the Motors "derived catalog summary" background
// worker. Every replica ticks, but only the one that wins a patterns.Mutex
// recomputes the summary (sqlc against the framework pool) and refreshes it in
// Valkey. GET /v1/catalog/summary then reads Valkey only — no Postgres on that
// path.
//
// This is deliberately NOT a job platform — see VPQ-TO-GENERAL-JOB-QUEUE.md.
// It is "one replica should refresh this shared derived cache" (VALKEY-HEAVY).
package catalogsummary

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"time"

	cf "github.com/caerus-framework/caerus-framework"
	cf_logs "github.com/caerus-framework/caerus-framework-logs"
	cf_postgres "github.com/caerus-framework/caerus-framework-postgresql"
	cf_valkey "github.com/caerus-framework/caerus-framework-valkey"
	"github.com/caerus-framework/caerus-framework-valkey/patterns"

	"github.com/caerus-framework/caerus-framework-demoapp/internal/db"
)

const componentName = "catalog-summary"

// Options are construction-time defaults.
type Options struct {
	// Interval between refresh attempts.
	Interval time.Duration
	// LockTTL bounds how long a replica may hold the refresh lock.
	LockTTL time.Duration
	// CacheTTL is how long the derived summary lives in Valkey.
	CacheTTL time.Duration
}

// Refresher is the Caerus Motors catalog-summary refresher (a Runnable).
type Refresher struct {
	mu       sync.Mutex
	interval time.Duration
	lockTTL  time.Duration
	cacheTTL time.Duration

	logger  *slog.Logger
	logsSub *cf_logs.Subscription
	queries *db.Queries
	valkey  *cf_valkey.CFValkey
}

// New creates the refresher (not yet started).
func New(opts Options) *Refresher {
	if opts.Interval <= 0 {
		opts.Interval = 15 * time.Second
	}
	if opts.LockTTL <= 0 {
		opts.LockTTL = 10 * time.Second
	}
	if opts.CacheTTL <= 0 {
		opts.CacheTTL = time.Minute
	}
	return &Refresher{
		interval: opts.Interval,
		lockTTL:  opts.LockTTL,
		cacheTTL: opts.CacheTTL,
		logger:   slog.Default(),
	}
}

// Name implements cf.CaerusComponent.
func (c *Refresher) Name() string { return componentName }

// GetInitOrderStage implements cf.CaerusComponent — data plane with postgres/valkey.
func (c *Refresher) GetInitOrderStage() cf.Stage { return cf_postgres.ComponentStage }

// GetDependencies implements cf.Dependencies.
func (c *Refresher) GetDependencies() []string {
	return []string{
		cf_logs.ComponentName,
		cf_postgres.ComponentName,
		cf_valkey.ComponentName,
	}
}

// Init resolves peers and builds the sqlc query set against the framework pool.
func (c *Refresher) Init(ctx context.Context, fw *cf.CaerusFramework) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if logs, ok := cf.Get[*cf_logs.Logs](fw); ok {
		c.logsSub = logs.OnReconfigureFor(c.Name(), func(l *slog.Logger) { c.logger = l })
	}

	pg, ok := cf.Get[*cf_postgres.CFPostgres](fw)
	if !ok {
		return errors.New("catalog-summary: postgresql component missing")
	}
	vk, ok := cf.Get[*cf_valkey.CFValkey](fw)
	if !ok {
		return errors.New("catalog-summary: valkey component missing")
	}
	c.queries = db.New(pg.Pool())
	c.valkey = vk
	c.logger.Info("catalog-summary: initialized",
		"interval", c.interval.String(),
		"lock_ttl", c.lockTTL.String(),
		"cache_ttl", c.cacheTTL.String(),
	)
	return nil
}

// Run implements cf.Runnable — ticks until ctx is canceled. Each tick tries to
// become the refresher via patterns.Mutex; losing replicas skip silently.
func (c *Refresher) Run(ctx context.Context) error {
	ticker := time.NewTicker(c.interval)
	defer ticker.Stop()

	c.tick(ctx)
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			c.tick(ctx)
		}
	}
}

func (c *Refresher) tick(ctx context.Context) {
	if err := ctx.Err(); err != nil {
		return
	}
	m := patterns.NewMutex(c.valkey, "catalog-summary", c.lockTTL)
	err := m.WithLock(ctx, func(ctx context.Context) error {
		rows, err := c.queries.VehiclesByMake(ctx)
		if err != nil {
			return err
		}
		total, err := c.queries.VehicleCount(ctx)
		if err != nil {
			return err
		}
		summary := map[string]any{"total": total, "by_make": rows}
		b, _ := json.Marshal(summary)
		key := c.valkey.Key("catalog", "summary")
		if err := c.valkey.Client().Do(ctx,
			c.valkey.Client().B().Set().Key(key).Value(string(b)).
				ExSeconds(int64(c.cacheTTL.Seconds())).Build()).Error(); err != nil {
			return err
		}
		c.logger.Info("catalog-summary: refreshed derived catalog",
			"key", key, "total", total)
		return nil
	})
	switch {
	case err == nil:
		// refreshed
	case errors.Is(err, patterns.ErrLocked):
		c.logger.Debug("catalog-summary: another replica holds the lock; skipping", "err", err)
	default:
		c.logger.Warn("catalog-summary: tick failed", "err", err)
	}
}

// Shutdown unsubscribes logs.
func (c *Refresher) Shutdown(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.logsSub != nil {
		c.logsSub.Unsubscribe()
		c.logsSub = nil
	}
	return nil
}

var (
	_ cf.CaerusComponent = (*Refresher)(nil)
	_ cf.Dependencies    = (*Refresher)(nil)
	_ cf.Runnable        = (*Refresher)(nil)
)

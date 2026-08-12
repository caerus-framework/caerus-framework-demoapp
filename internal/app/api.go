// Package app is the Motors application component: HTTP API plus product-owned
// subcomponents (interest VPQ, catalog-summary refresher).
//
// Why a Runnable (not Listen in main)? So RunWithSignals owns cancellation:
// SIGTERM → ctx cancel → Shutdown on the HTTP server → framework teardown in
// reverse Init order. That is the difference between “demo that dies on kill -9
// only” and “service that matches production shutdown”.
//
// Subcomponents() exposes children constructed in New; the framework expands
// them into the registry (EGG.md). main only declares chassis + app.New.
//
// The app also owns the ops jobs (--demoapp.job=seed|doctor|price) via
// JobRunner: the framework's job-only path initializes this component's
// dependency closure (core + the whole data plane) and runs the named task
// without starting any background runners.
package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	cf "github.com/caerus-framework/caerus-framework"
	cf_configuration "github.com/caerus-framework/caerus-framework-configuration"
	cf_logs "github.com/caerus-framework/caerus-framework-logs"
	cf_postgres "github.com/caerus-framework/caerus-framework-postgresql"
	cf_valkey "github.com/caerus-framework/caerus-framework-valkey"
	"github.com/caerus-framework/caerus-framework-valkey/patterns"
	cf_vpq "github.com/caerus-framework/caerus-framework-vpq"
	"github.com/google/uuid"
	"github.com/valkey-io/valkey-go"
	"golang.org/x/sync/singleflight"

	"github.com/caerus-framework/caerus-framework-demoapp/internal/catalogsummary"
	"github.com/caerus-framework/caerus-framework-demoapp/internal/store"
)

const componentName = "motors-api"

// ComponentStage is the app's own plane, above the data plane: components in
// earlier stages (logs, configuration, observability, postgres, valkey,
// interest VPQ, catalog-summary) initialize first, and a job targeting this
// component pulls in that whole closure.
const ComponentStage = cf.Stage("app")

// Options are construction-time defaults; Init may overlay demoapp.json.
type Options struct {
	HTTPAddr         string
	PriceCacheTTLSec int
	// ConfigSource / ConfigPath name where this app class's own configuration
	// source is registered. New registers it with the configuration component
	// via cf.ConfigSourceRegistrar (source "demoapp", file config/demoapp.json,
	// env DEMOAPP_, owner "motors-api", flags --http-addr / --vpq-debug).
	ConfigSource string
	ConfigPath   string
}

// API is the Caerus Motors HTTP surface and owns product subcomponents.
type API struct {
	mu           sync.Mutex
	httpAddr     string
	cacheTTL     time.Duration
	configSource string
	configPath   string

	fw      *cf.CaerusFramework
	pg      *cf_postgres.CFPostgres
	logger  *slog.Logger
	logsSub *cf_logs.Subscription
	store   *store.Store
	valkey  *cf_valkey.CFValkey
	queue   *cf_vpq.PriorityQueue
	server  *http.Server

	// Owned children (constructed in New; registered via Subcomponents).
	interest *cf_vpq.PriorityQueue
	catalog  *catalogsummary.Refresher

	// interestLog is the logger for the "interest" VPQ consumer (see
	// InterestHandler). It is a separate subscription under the "interest" name
	// so --vpq-debug / logs.SetLevelFor("interest", …) turns on its debug logs.
	interestLog atomic.Pointer[slog.Logger]
	interestSub *cf_logs.Subscription

	// priceGroup coalesces concurrent cold-cache reads inside this process
	// (singleflight) — see handleGetPrice.
	priceGroup singleflight.Group
}

// New creates the API component and its product-owned children (not yet Init'd).
func New(opts Options) *API {
	if opts.HTTPAddr == "" {
		opts.HTTPAddr = ":8081"
	}
	if opts.PriceCacheTTLSec <= 0 {
		opts.PriceCacheTTLSec = 60
	}
	if opts.ConfigSource == "" {
		opts.ConfigSource = "demoapp"
	}
	if opts.ConfigPath == "" {
		opts.ConfigPath = "config/demoapp.json"
	}
	a := &API{
		httpAddr:     opts.HTTPAddr,
		cacheTTL:     time.Duration(opts.PriceCacheTTLSec) * time.Second,
		configSource: opts.ConfigSource,
		configPath:   opts.ConfigPath,
		logger:       slog.Default(),
	}
	a.interestLog.Store(slog.Default())
	// Interest heat queue — product-owned; handler closes over this API.
	a.interest = cf_vpq.New(
		cf_vpq.WithName("interest"),
		cf_vpq.WithQueueName("interest"),
		cf_vpq.WithKeyPrefix("demo:"),
		cf_vpq.WithWorkers(2),
		cf_vpq.WithHandler(a.InterestHandler()),
	)
	a.catalog = catalogsummary.New(catalogsummary.Options{})
	return a
}

// Subcomponents implements cf.Subcomponents — interest VPQ and catalog-summary
// refresher are flattened into the framework registry by AddComponent.
func (a *API) Subcomponents() []cf.CaerusComponent {
	return []cf.CaerusComponent{a.interest, a.catalog}
}

// Name implements cf.CaerusComponent.
func (a *API) Name() string { return componentName }

// RegisterConfigSources implements cf.ConfigSourceRegistrar. The framework
// calls it during argv absorption so this app class owns its configuration
// source: type DemoAppConfig, file ConfigPath, env DEMOAPP_, owner "motors-api".
// The --http-addr / --vpq-debug flags (DemoAppConfig flag tags) are registered
// by ParseFlags once this source exists.
func (a *API) RegisterConfigSources(conf any) error {
	cfg, ok := conf.(*cf_configuration.Configuration)
	if !ok {
		return fmt.Errorf("motors-api: RegisterConfigSources: expected configuration component, got %T", conf)
	}
	return cf_configuration.AddSource(cfg, cf_configuration.Source[DemoAppConfig]{
		Name:      a.configSource,
		Path:      a.configPath,
		Format:    cf_configuration.FormatJSON,
		EnvPrefix: "DEMOAPP_",
		Owner:     a.Name(),
		// CLI-only ops jobs: --demoapp.job=seed|doctor|price. The flag names the
		// source (and thus this component), the value names the task (RunJob).
		Job: cf.JobSpec{Flag: a.configSource + ".job", Tasks: []string{"seed", "doctor", "price"}},
	})
}

// GetInitOrderStage implements cf.CaerusComponent — the app plane, above data.
func (a *API) GetInitOrderStage() cf.Stage { return ComponentStage }

// GetDependencies implements cf.Dependencies.
//
// We depend on the *named* VPQ instance "interest" (WithName), not the default
// "vpq". Get[T] would be ambiguous if more queues appeared later. The
// configuration component is resolved during Init to read the demoapp source.
func (a *API) GetDependencies() []string {
	return []string{
		cf_logs.ComponentName,
		cf_configuration.ComponentName,
		cf_postgres.ComponentName,
		cf_valkey.ComponentName,
		"interest",
	}
}

// Init resolves peers. Pool/Client must already be live (deps Init first).
func (a *API) Init(ctx context.Context, fw *cf.CaerusFramework) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	a.fw = fw

	var logs *cf_logs.Logs
	if l, ok := cf.Get[*cf_logs.Logs](fw); ok {
		logs = l
		a.logsSub = l.OnReconfigureFor(a.Name(), func(l *slog.Logger) { a.logger = l })
		a.interestSub = l.OnReconfigureFor("interest", func(l *slog.Logger) { a.interestLog.Store(l) })
	}

	// Optional: refresh listen addr / TTL from configuration after sources loaded.
	// Serve-time settings live on the app class; main only declares and runs.
	if conf, ok := cf.Get[*cf_configuration.Configuration](fw); ok {
		if cfg, err := cf_configuration.Lookup[DemoAppConfig](conf, a.configSource); err == nil {
			if cfg.HTTPAddr != "" {
				a.httpAddr = cfg.HTTPAddr
			}
			if cfg.PriceCacheTTLSec > 0 {
				a.cacheTTL = time.Duration(cfg.PriceCacheTTLSec) * time.Second
			}
			if cfg.VPQDebug && logs != nil {
				logs.SetLevelFor("interest", slog.LevelDebug)
				a.logger.Info("demoapp: SetLevelFor(interest, DEBUG) via vpq_debug (file / DEMOAPP_VPQ_DEBUG / --vpq-debug)")
			}
		}
	}

	pg, ok := cf.Get[*cf_postgres.CFPostgres](fw)
	if !ok {
		return errors.New("motors-api: postgresql component missing")
	}
	a.pg = pg
	a.store = store.New(pg.Pool())

	vk, ok := cf.Get[*cf_valkey.CFValkey](fw)
	if !ok {
		return errors.New("motors-api: valkey component missing")
	}
	a.valkey = vk

	q, ok := cf.GetByName[*cf_vpq.PriorityQueue](fw, "interest")
	if !ok {
		return errors.New("motors-api: interest VPQ missing (WithName(\"interest\"))")
	}
	a.queue = q

	mux := http.NewServeMux()
	a.routes(mux)
	a.server = &http.Server{Addr: a.httpAddr, Handler: mux}

	a.logger.Info("motors-api: initialized",
		"http_addr", a.httpAddr,
		"price_cache_ttl", a.cacheTTL.String(),
	)
	return nil
}

// Run implements cf.Runnable and is serve-only: it blocks until ctx cancel or a
// Listen error. The ops shapes are jobs, not subcommands — RunJob routes
// --demoapp.job=seed|doctor|price. This component's dependency closure (core +
// the whole data plane) initializes before either path, so the job tasks see a
// live chassis while the serve path never starts background runners.
func (a *API) Run(ctx context.Context) error {
	a.mu.Lock()
	srv := a.server
	addr := a.httpAddr
	a.mu.Unlock()
	if srv == nil {
		return errors.New("motors-api: Run before Init")
	}

	errCh := make(chan error, 1)
	go func() {
		a.logger.Info("motors-api: listening", "addr", addr)
		err := srv.ListenAndServe()
		if errors.Is(err, http.ErrServerClosed) {
			errCh <- nil
			return
		}
		errCh <- err
	}()

	select {
	case <-ctx.Done():
		// Fresh context: parent is already canceled.
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
		return <-errCh
	case err := <-errCh:
		return err
	}
}

// RunJob implements cf.JobRunner. Tasks are the ops shapes behind the
// --demoapp.job flag: seed, doctor, price. The job-only init path initializes
// this component's dependency closure first; the tasks return nil when done and
// the framework tears down and exits.
func (a *API) RunJob(ctx context.Context, task string) error {
	switch task {
	case "seed":
		return a.seed(ctx)
	case "doctor":
		return a.doctor(ctx)
	case "price":
		if a.fw == nil {
			return errors.New("motors-api: price job before Init")
		}
		return a.price(ctx, a.fw.LeftoverArgs())
	default:
		return fmt.Errorf("motors-api: unknown job task %q (want seed|doctor|price)", task)
	}
}

// seed writes the demo data set.
func (a *API) seed(ctx context.Context) error {
	if err := a.store.SeedLotsVehiclesPrices(ctx); err != nil {
		return err
	}
	fmt.Fprintln(os.Stderr, "demoapp: seeded lots + vehicles (incl. Porsche 911) + prices")
	return nil
}

// doctor prints a connectivity report.
func (a *API) doctor(ctx context.Context) error {
	fmt.Println("demoapp doctor")
	if a.pg != nil {
		fmt.Printf("  postgresql: %s\n", healthLine(a.pg.Health(ctx)))
	}
	if a.valkey != nil {
		fmt.Printf("  valkey:     %s\n", healthLine(a.valkey.Health(ctx)))
	}
	fmt.Println("  tip: after serve → curl -sS localhost:9090/readyz")
	fmt.Println("  tip: dirty schema_migrations → golang-migrate force VERSION (Caerus does not auto-force)")
	return nil
}

// price reads or writes a vehicle price. Positional args come from
// fw.LeftoverArgs(): --demoapp.job=price get <uuid> | set <uuid> <cents>.
func (a *API) price(ctx context.Context, args []string) error {
	if len(args) < 2 {
		return errors.New("usage: --demoapp.job=price get <vehicle-uuid> | set <vehicle-uuid> <cents>")
	}
	op := args[0]
	id, err := uuid.Parse(args[1])
	if err != nil {
		return fmt.Errorf("vehicle uuid: %w", err)
	}
	switch op {
	case "get":
		p, err := a.store.GetPrice(ctx, id)
		if err != nil {
			return err
		}
		b, _ := json.MarshalIndent(p, "", "  ")
		fmt.Println(string(b))
		if client := a.valkey.Client(); client != nil {
			key := a.valkey.Key("price", id.String())
			raw, err := client.Do(ctx, client.B().Get().Key(key).Build()).ToString()
			if err == nil {
				fmt.Println("valkey", key, "=", raw)
			} else {
				fmt.Println("valkey", key, "= (miss)")
			}
		}
		return nil
	case "set":
		if len(args) < 3 {
			return errors.New("usage: --demoapp.job=price set <vehicle-uuid> <cents>")
		}
		cents, err := strconv.ParseInt(args[2], 10, 64)
		if err != nil {
			return fmt.Errorf("price cents: %w", err)
		}
		p, err := a.store.UpsertPrice(ctx, id, cents, "USD")
		if err != nil {
			return err
		}
		if client := a.valkey.Client(); client != nil {
			key := a.valkey.Key("price", id.String())
			b, _ := json.Marshal(p)
			_ = client.Do(ctx, client.B().Set().Key(key).Value(string(b)).ExSeconds(int64(a.cacheTTL.Seconds())).Build()).Error()
		}
		fmt.Printf("set %s → %d %s\n", id, p.AmountCents, p.Currency)
		return nil
	default:
		return fmt.Errorf("unknown price op %q", op)
	}
}

// InterestHandler returns the VPQ handler for the "interest" queue. It logs
// through the framework logger subscribed under the "interest" name so
// --vpq-debug / logs.SetLevelFor("interest", …) controls its verbosity.
func (a *API) InterestHandler() func(*cf_vpq.BGetObject) error {
	return func(item *cf_vpq.BGetObject) error {
		l := a.interestLog.Load()
		if l == nil {
			l = slog.Default()
		}
		l.Info("interest heat: follow up this vehicle",
			"vehicle_id", item.ObjectID,
			"weight", item.ObjectScore,
			"payload", item.ObjectValue,
		)
		return nil
	}
}

func healthLine(err error) string {
	if err == nil {
		return "ok"
	}
	return "DOWN (" + err.Error() + ")"
}

// Shutdown unsubscribes logs. HTTP server is stopped in Run on cancel.
func (a *API) Shutdown(ctx context.Context) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.logsSub != nil {
		a.logsSub.Unsubscribe()
		a.logsSub = nil
	}
	if a.interestSub != nil {
		a.interestSub.Unsubscribe()
		a.interestSub = nil
	}
	return nil
}

func (a *API) routes(mux *http.ServeMux) {
	// App-level livez (process up). Prefer observability :9090/readyz for k8s readiness.
	mux.HandleFunc("GET /livez", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("GET /v1/lots", a.handleListLots)
	mux.HandleFunc("POST /v1/lots", a.handleCreateLot)
	mux.HandleFunc("GET /v1/vehicles", a.handleListVehicles)
	mux.HandleFunc("POST /v1/vehicles", a.handleCreateVehicle)
	mux.HandleFunc("GET /v1/prices/{id}", a.handleGetPrice)
	mux.HandleFunc("PUT /v1/prices/{id}", a.handlePutPrice)
	mux.HandleFunc("POST /v1/interest/{id}", a.handleInterest)
	mux.HandleFunc("GET /v1/catalog/summary", a.handleCatalogSummary)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func (a *API) handleListLots(w http.ResponseWriter, r *http.Request) {
	lots, err := a.store.ListLots(r.Context())
	if err != nil {
		a.logger.Error("list lots", "err", err)
		writeErr(w, http.StatusInternalServerError, "list lots failed")
		return
	}
	if lots == nil {
		lots = []store.Lot{}
	}
	writeJSON(w, http.StatusOK, lots)
}

func (a *API) handleCreateLot(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name    string `json:"name"`
		Address string `json:"address"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Name == "" {
		writeErr(w, http.StatusBadRequest, "need JSON {name, address?}")
		return
	}
	lot, err := a.store.CreateLot(r.Context(), body.Name, body.Address)
	if err != nil {
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, lot)
}

func (a *API) handleListVehicles(w http.ResponseWriter, r *http.Request) {
	vs, err := a.store.ListVehicles(r.Context())
	if err != nil {
		a.logger.Error("list vehicles", "err", err)
		writeErr(w, http.StatusInternalServerError, "list vehicles failed")
		return
	}
	if vs == nil {
		vs = []store.Vehicle{}
	}
	writeJSON(w, http.StatusOK, vs)
}

func (a *API) handleCreateVehicle(w http.ResponseWriter, r *http.Request) {
	var body struct {
		VIN   string  `json:"vin"`
		Make  string  `json:"make"`
		Model string  `json:"model"`
		Year  int     `json:"year"`
		Color string  `json:"color"`
		LotID *string `json:"lot_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.VIN == "" || body.Make == "" || body.Model == "" || body.Year == 0 {
		writeErr(w, http.StatusBadRequest, "need JSON {vin, make, model, year, color?, lot_id?}")
		return
	}
	var lotID *uuid.UUID
	if body.LotID != nil && *body.LotID != "" {
		id, err := uuid.Parse(*body.LotID)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "lot_id must be a UUID")
			return
		}
		lotID = &id
	}
	v, err := a.store.CreateVehicle(r.Context(), body.VIN, body.Make, body.Model, body.Year, body.Color, lotID)
	if err != nil {
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, v)
}

// handleGetPrice demonstrates cache-aside with in-process stampede protection:
//  1. Try Valkey (fast path) — X-Cache: HIT
//  2. On miss, patterns.GetOrLoad runs the Postgres read once and concurrent
//     callers in this process wait on that single load (singleflight).
//  3. The result is stored with TTL; the waiter is still labeled HIT (the DB
//     was hit once, not per-request).
//
// If Valkey is down, we still serve from Postgres — cache is an acceleration,
// not a correctness dependency. (/readyz will still fail because valkey Health fails.)
func (a *API) handleGetPrice(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "id must be a vehicle UUID")
		return
	}
	ctx := r.Context()
	key := a.valkey.Key("price", id.String())
	client := a.valkey.Client()

	if client != nil {
		raw, gerr := client.Do(ctx, client.B().Get().Key(key).Build()).ToString()
		if gerr == nil && raw != "" {
			var p store.Price
			if json.Unmarshal([]byte(raw), &p) == nil {
				w.Header().Set("X-Cache", "HIT")
				writeJSON(w, http.StatusOK, p)
				return
			}
		}
		if gerr != nil && !errors.Is(gerr, valkey.Nil) {
			// Valkey unreachable: bypass the cache entirely (no silent half-writes).
			p, err := a.store.GetPrice(ctx, id)
			if errors.Is(err, store.ErrNotFound) {
				writeErr(w, http.StatusNotFound, "no price for vehicle")
				return
			}
			if err != nil {
				writeErr(w, http.StatusInternalServerError, err.Error())
				return
			}
			w.Header().Set("X-Cache", "DOWN")
			writeJSON(w, http.StatusOK, p)
			return
		}
	}

	b, shared, err := patterns.GetOrLoad(ctx, a.valkey, &a.priceGroup, "price:"+id.String(), a.cacheTTL,
		func(ctx context.Context) ([]byte, error) {
			p, err := a.store.GetPrice(ctx, id)
			if err != nil {
				return nil, err
			}
			return json.Marshal(p)
		})
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, http.StatusNotFound, "no price for vehicle")
			return
		}
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	var p store.Price
	_ = json.Unmarshal(b, &p)
	cacheState := "MISS"
	if shared {
		// A concurrent request loaded it while we waited — the stampede became
		// one query. The value is warm now, so label the header HIT.
		cacheState = "HIT"
	}
	w.Header().Set("X-Cache", cacheState)
	writeJSON(w, http.StatusOK, p)
}

func (a *API) handlePutPrice(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "id must be a vehicle UUID")
		return
	}
	var body struct {
		AmountCents int64  `json:"amount_cents"`
		Currency    string `json:"currency"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "need JSON {amount_cents, currency?}")
		return
	}
	p, err := a.store.UpsertPrice(r.Context(), id, body.AmountCents, body.Currency)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	// Write-through: update cache immediately so GET doesn't briefly show stale $.
	if client := a.valkey.Client(); client != nil {
		key := a.valkey.Key("price", id.String())
		b, _ := json.Marshal(p)
		_ = client.Do(r.Context(), client.B().Set().Key(key).Value(string(b)).ExSeconds(int64(a.cacheTTL.Seconds())).Build()).Error()
	}
	writeJSON(w, http.StatusOK, p)
}

// handleCatalogSummary serves the derived catalog summary from Valkey
// (internal/catalogsummary.Refresher owns recomputation behind a patterns.Mutex —
// the API never touches Postgres on this path). 404 means the refresher has
// not completed its first successful pass yet.
func (a *API) handleCatalogSummary(w http.ResponseWriter, r *http.Request) {
	client := a.valkey.Client()
	if client == nil {
		writeErr(w, http.StatusServiceUnavailable, "valkey unavailable")
		return
	}
	raw, err := client.Do(r.Context(), client.B().Get().Key(a.valkey.Key("catalog", "summary")).Build()).ToString()
	if err != nil {
		writeErr(w, http.StatusNotFound, "catalog summary not refreshed yet")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(raw))
}

// handleInterest bumps weighted priority for a vehicle. Call it several times
// for the Porsche, once for the Corolla — then watch which id the VPQ handler logs first.
func (a *API) handleInterest(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "id must be a vehicle UUID")
		return
	}
	ok, err := a.store.VehicleExists(r.Context(), id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !ok {
		writeErr(w, http.StatusNotFound, "unknown vehicle")
		return
	}
	payload := fmt.Sprintf(`{"vehicle_id":%q,"note":"demo interest ping"}`, id.String())
	added, err := a.queue.Add(r.Context(), id.String(), payload)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{
		"vehicle_id": id,
		"queued":     true,
		"new_id":     added, // false means weight increased on an existing queue member
		"hint":       "POST again to raise weight — hottest car pops first on the interest queue",
	})
}

var (
	_ cf.CaerusComponent       = (*API)(nil)
	_ cf.Dependencies          = (*API)(nil)
	_ cf.Subcomponents         = (*API)(nil)
	_ cf.Runnable              = (*API)(nil)
	_ cf.JobRunner             = (*API)(nil)
	_ cf.ConfigSourceRegistrar = (*API)(nil)
)

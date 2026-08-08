# Caerus Motors — framework demoapp

**This is not a dealership product.** It is a disposable, cars-themed playground that shows how a real Caerus service is wired: logs → configuration → observability → Postgres → Valkey → VPQ → your HTTP Runnable.

If you only have fifteen minutes, do this:

```bash
cd caerus-framework-demoapp
make up && make migrate && make seed && make run
# other terminal:
make curl-demo
```

You should see lots, a Porsche 911 on the downtown lot, `X-Cache: HIT` on the second price GET, interest-heat log lines when VPQ pops the hottest car, and a derived catalog summary refreshed into Valkey.

The *why* walkthrough (read when confused or tired) is
[`docs/WALKTHROUGH.md`](docs/WALKTHROUGH.md).

---

## Why this exists (read even if skimming)

Framework docs can feel like airport signage: correct, cold, easy to walk past. This app is the opposite on purpose.

| Feeling you might have | What the demo is answering |
|---|---|
| “Where do I put migrate?” | Local `serve` uses `WithMigrateOnInit`. Prod-shaped path is the framework Job: `--postgresql.job=migrate` as a Job, then serve **without** migrate-on-init. |
| “Is Valkey required for correctness?” | No for price *truth* (Postgres wins). Yes for `/readyz` — if cache is down we still want k8s to stop sending traffic if you declared the dep. |
| “Can I use VPQ as a job queue?” | No. Interest heat is weighted priority. No DLQ, no cron — use River/asynq/NATS for that. |
| “Why is the API on :8081?” | `:9090` is observability (`/readyz`, `/metrics`). Mixing them teaches the wrong production habit. |
| “Why `SetLevelFor(\"interest\")` not `\"vpq\"`?” | We registered `WithName("interest")`. Per-component levels key off **Name()**. |

---

## Ports & processes

| Port | Owner | Endpoints |
|---|---|---|
| **9090** | `caerus-framework-observability` | `/livez`, `/readyz`, `/metrics` |
| **8081** | Motors API (`motors-api` Runnable) | `/livez`, `/v1/...` |
| **5432** | Compose Postgres | DSN via `config/postgresql.json` or `POSTGRES_DSN` |
| **6379** | Compose Valkey | `config/valkey.json` or `VALKEY_URL` |

---

## Commands

Every one-shot is a framework **job** — the flag names the instance, the value names the task. No subcommands exist; `make X` targets just expand to the flag.

| Command | Expands to | What it does |
|---|---|---|
| `make up` / `make down` | — | Postgres + Valkey only (always first locally) |
| `make migrate` | `--postgresql.job=migrate` | Init postgres + closure → Migrate → exit |
| `make seed` | `--demoapp.job=seed` | Idempotent lots / cars / prices (safe to re-run) |
| `make doctor` | `--demoapp.job=doctor` | Ping deps + print tips |
| `make run` | `serve` | `serve` with **migrate-on-init ON** (laptop) |
| `make curl-demo` | — | Happy-path curls (after `run`) |
| `--demoapp.job=price get\|set` | — | CLI price without HTTP (debugging cache vs DB) |

Env knobs (optional):

```bash
export POSTGRES_DSN='postgres://demo:demo@127.0.0.1:5432/demo?sslmode=disable'
export VALKEY_URL='redis://127.0.0.1:6379'
```

Flag knobs (optional; interspersed GNU-style — flags are extracted wherever they appear, e.g. `demoapp serve --http-addr :8082` or `serve --vpq-debug`):

| Flag | Maps to |
|---|---|
| `--postgresql <path>` | File for the postgresql source (default `config/postgresql.json`) |
| `--valkey <path>` | File for the valkey source (default `config/valkey.json`) |
| `--demoapp <path>` | File for the demoapp source (default `config/demoapp.json`) |
| `--http-addr <addr>` | `DemoAppConfig.HTTPAddr` (default `:8081`) |
| `--vpq-debug` | `DemoAppConfig.VPQDebug` → `SetLevelFor("interest", DEBUG)` |
| `--postgresql.job=migrate` | Job: init postgres + closure → migrate → exit |
| `--demoapp.job=seed\|doctor\|price` | Job: init app + closure → run task → exit |

Config layering (later wins): **file → `POSTGRES_` / `VALKEY_` / `LOGS_` / `DEMOAPP_` env → `--<flag>` → `AfterLoad` DSN/URL overlays**. That is the Caerus production path; the demo uses it on purpose. Declare-and-fill: every option is declared on the typed `Source[T]` structs — postgres/valkey register their own sources (`WithConfigSource(name, path)`), the Motors API registers the `demoapp` source (`internal/app`), logs/observability self-register via `cf.CoreConfigSource`, and the framework **absorbs argv itself** (registrar pass → core-source declarations → `ParseFlags`) before serving. Wiring: `cmd/demoapp/main.go` declares `cf.New(&cf.FrameworkOptions{…})` top-to-bottom (auto-registered core + declared chassis + app classes) and calls `RunWithSignals` — no `Getenv`, no `ParseFlags`, no `registerSources`, no verb switch, no subcommands. Every process shape is a job flag declared by the owning component: the app declares `--demoapp.job` with tasks `seed`/`doctor`/`price`; postgres declares `--postgresql.job` with task `migrate`. `RunWithSignals` asks configuration (`cf.JobSource`) whether a flag was set; when it was, it initializes the **target's dependency closure** — its plane and everything below it (data-level `migrate` pulls in core + postgres only; app-level `seed`/`doctor`/`price` pull in the whole data plane) — runs the task, tears down, exits. Jobs never start background runners.

---

## HTTP surface (Motors API :8081)

| Method | Path | Notes |
|---|---|---|
| GET | `/livez` | Process up (app-level). Prefer `:9090/readyz` for k8s. |
| GET/POST | `/v1/lots` | Lot CRUD (list + create) |
| GET/POST | `/v1/vehicles` | Fleet list includes lot name + price when present |
| GET/PUT | `/v1/prices/{vehicle_id}` | Cache-aside GET (`X-Cache: HIT/MISS/DOWN`); write-through PUT |
| POST | `/v1/interest/{vehicle_id}` | VPQ `Add` — repeat to raise weight |
| GET | `/v1/catalog/summary` | Derived catalog summary from Valkey (no Postgres on this path) |

Observability (:9090) is registered as a framework component — you do **not** re-implement readiness in the Motors mux.

---

## Migrate policy (do not skip)

Caerus locked **C+B**:

1. **Production multi-replica:** run migrate as a Job (`--postgresql.job=migrate`). Serving Deployments omit `WithMigrateOnInit`.
2. **Local demo `serve`:** `WithMigrateOnInit` is ON so `make run` is one command after Compose.
3. **Dirty `schema_migrations`:** Caerus will **not** auto-`force`. Operator runs golang-migrate `force VERSION` after deciding what is true. See doctor tip.

If Init fails with a dirty error, that is intentional friction — better than silent schema lies.

---

## Layout (where to look when lost)

```text
cmd/demoapp/              wiring: FrameworkOptions (chassis + app.New) — main never
                          declares VPQ/catalog-summary; no argv dispatch / subcommands
internal/app/             Motors app: HTTP + DemoAppConfig + jobs + Subcomponents
                          (interest VPQ, catalog-summary refresher)
internal/catalogsummary/  derived-catalog refresher (patterns.Mutex + sqlc)
internal/db/              sqlc-generated queries (read side) — schema = migrate .up.sql
internal/store/           hand-written pgx SQL (writes + price reads) — no GORM
internal/dbmigrate/       embedded *.up.sql / *.down.sql — passed as dbmigrate.Migrations
```

---

## Seed fleet

| VIN | Car | Lot | Price |
|---|---|---|---|
| DEMO-VIN-001 | Volvo EX30 | downtown | $42,990 |
| DEMO-VIN-002 | Toyota Corolla | airport | $15,990 |
| DEMO-VIN-003 | Ford F-150 | warehouse | $38,990 |
| DEMO-VIN-004 | Tesla Model 3 | downtown | $35,990 |
| DEMO-VIN-005 | Porsche 911 Carrera | downtown | $129,900 |

Demo VINs are strings for teaching, not ISO-3779 identifiers.

---

## Interest heat (VPQ) in one paragraph

`POST /v1/interest/{id}` calls `queue.Add`. Adding the **same** vehicle again increases weight. Workers pop hottest first and log `interest heat: follow up this vehicle`. The handler is idempotent on purpose (log only) so you learn the queue mechanic without inventing a fake CRM. Loud internals: `demoapp serve --vpq-debug` or `"vpq_debug": true` in `config/demoapp.json` (or `DEMOAPP_VPQ_DEBUG=1`).

## Derived catalog: one replica recomputes, all read (VALKEY-HEAVY Phase 4)

`internal/catalogsummary` is an app-owned Runnable (via `Subcomponents`). Every
replica ticks every 15s, but only the one that wins a `patterns.Mutex`
(`demo:catalog-summary`) runs the sqlc `VehiclesByMake` / `VehicleCount` queries
over `cf_postgres.Pool()` and refreshes `demo:catalog:summary` in Valkey (TTL 1m).
Losers skip (`patterns.ErrLocked`). The API's `GET /v1/catalog/summary` is a
pure Valkey read — this is a shared derived cache, not a job platform.

Try it with two replicas on different ports and watch `/metrics`:
`lock_acquire_ok_total` only counts the winners; `postgresql_pool_acquire_total`
barely moves on the read path.

The price path shows the other patterns helper: `patterns.GetOrLoad` collapses
concurrent cold-cache reads in this process to a single Postgres query
(caller-owned `singleflight.Group`). `X-Cache` stays meaningful — `MISS` on a
load, `HIT` on a warm/waiter read, `DOWN` when Valkey is unreachable and we
serve straight from Postgres (cache is acceleration, not correctness).

---

## License

Apache License 2.0 — see [LICENSE](LICENSE).

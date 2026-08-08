# Walkthrough — what is going on, and why it is like that

You are tired. The README was a map. This file is the tour guide who keeps talking so you do not wander into the gift shop and buy GORM.

Read in order the first time. Later, Ctrl-F the pain you have right now.

---

## 0. The one-sentence model

**Caerus owns process lifecycle and ops chassis; your app owns SQL and HTTP handlers.**

Postgres pool, migrate hooks, Valkey client, VPQ workers, `/readyz`, log rebuilds — framework.  
`SELECT`/`INSERT`, JSON shapes, “bump interest on this car” — you (`internal/store`, `internal/app`).

If a PR starts putting business SQL inside a framework module, stop. That is the wrong direction — keep queries in the app (`internal/store`).

---

## 1. Bootstrap order is a story, not a ritual

Open [`cmd/demoapp/main.go`](../cmd/demoapp/main.go) and read top-to-bottom — the wiring lives in `main`: the declaration (`cf.New(&cf.FrameworkOptions{…})`) + `RunWithSignals`. There is no dispatch anywhere — every process shape is a job flag declared by the owning component (`--postgresql.job=migrate`, `--demoapp.job=seed|doctor|price`). `cf.New` registers the always-on core (logs → configuration → observability) then the declared `Components` list, in order:

1. **logs** — everything else needs a logger that can be rebuilt.
2. **configuration** — files + env + DSN overlays, loaded at `AddSource` time (yes, before `Init`).
3. **observability** — probes on `:9090`; bound to `config/observability.json` via `ObservabilitySettings.ConfigSource`.
4. **postgresql** — pool + migrations (± migrate-on-init); registers its own `postgresql` config source.
5. **valkey** — cache + VPQ backend; registers its own `valkey` config source.
6. **interest VPQ** — weighted queue named `"interest"` (not `"vpq"`).
7. **motors-api** — your Runnable, in the **app plane** (its own stage above "data"); declares the `demoapp` config source with the ops jobs `--demoapp.job=seed|doctor|price`.

Stages matter: postgres/valkey/vpq/api all declare the `"data"` stage
(`cf_postgres.ComponentStage`), which `AddComponent` registers automatically —
there is no explicit stage API. Dependencies inside the stage (`GetDependencies`) keep “API after pool” honest even if someone reorders the `Components` list.

**Why you should care when half-asleep:** if Init fails, the error almost always means “a peer this component declared is missing or not ready” — not “Go is haunted.”

---

## 2. Configuration layering (file → env → DSN)

You will be tempted to hardcode DSNs in `main`. Do not. The demo shows the agreed path:

```text
config/postgresql.json     defaults for Compose
    ↓
POSTGRES_HOST / …          env prefix overlay (local/CI knobs)
    ↓
POSTGRES_DSN AfterLoad     one URL wins for host networking / k8s secrets style
```

Same idea for Valkey with `VALKEY_URL`.

**Why AfterLoad exists:** JSON cannot express “sometimes I only have a URL.” Overlay helpers (`OverlayDSN`, `OverlayURL`) merge URL fields into the typed config struct without inventing a second config type. In the demo these hooks live **inside the modules** (postgres/valkey self-register their sources, logs/observability self-register via `cf.CoreConfigSource`); `main` never touches `os.Getenv`.

The framework absorbs argv itself: components register their sources
(`cf.ConfigSourceRegistrar`, core via `cf.CoreConfigSource`), then
`ParseFlags` runs — so `--http-addr`,
`--vpq-debug` and the per-source `--<name>` file flags work with zero
`main` plumbing. `Lookup` works **after** `AddSource` because `AddSource`
fail-fast loads immediately.

---

## 3. Migrate: two doors, one schema

| Door | Code | Use |
|---|---|---|
| A — local serve | `WithMigrateOnInit()` (on in the demo binary) | Laptop `make run` |
| B — Job | `--postgresql.job=migrate` → `RunWithSignals` job path (no `migrate` subcommand) | Prod Jobs, CI, explicit ops |

Both need a migrations FS. Door A without migrations is a hard Init error (framework enforces that — good). Door B initializes the target's **dependency closure** — for postgres that's the core plus postgres only (no HTTP, no catalog-summary) — runs `Migrate`, exits.

**Dirty database:** if a previous migrate crashed mid-way, golang-migrate marks dirty. Caerus does **not** call `force` for you. That is not laziness; automatic force is how you get two replicas “fixing” different realities. Doctor prints the tip on purpose.

Production consumer pattern (auth-api, etc.): **Job migrates, Deployment serves, never migrate-on-init in the Deployment.**

---

## 4. Embed path gotcha (why migrations live under `internal/dbmigrate`)

Go’s `//go:embed` **cannot** use `..`. So we do not keep SQL only at module root and embed from `cmd/`. They live next to [`internal/dbmigrate/fs.go`](../internal/dbmigrate/fs.go). Mentally still call them “the migrations.” Physically they must sit where embed can see them.

`dbmigrate.Migrations` is the raw `embed.FS`; `cf_postgres.WithEmbeddedMigrations(dbmigrate.Migrations, "migrations")` resolves the sub-filesystem inside the component — no `fs.Sub` or error handling in `main`.

If you move them and build breaks with embed errors, you hit this rule again.

---

## 5. Cache-aside without lying to yourself

Price GET ([`internal/app/api.go`](../internal/app/api.go)):

1. Try Valkey key `demo:price:{uuid}` (prefix from Valkey component).
2. Miss → Postgres (source of truth).
3. Fill Valkey with TTL.
4. Headers: `X-Cache: HIT|MISS` so `curl -D -` teaches without a debugger.

PUT writes Postgres then write-through to Valkey so the next reader does not briefly see yesterday’s sticker price.

**If Valkey is down:** GET still works from Postgres. `/readyz` still fails if Valkey is a declared HealthProvider — readiness is “should this replica take traffic,” not “can I limp.” That tension is intentional; tune health checks when you consciously accept degraded mode.

---

## 6. Interest heat is not a job platform

VPQ = Valkey Priority Queue. Same `object_id` added again → weight up → pops sooner.

The demo handler **only logs**. That is not incomplete; that is the lesson boundary. The moment you add email/CRM side effects without idempotency keys, you are building a job system and VPQ will hurt you — reach for River/asynq/NATS before “just one more feature.”

Debug levels:

```bash
DEMOAPP_VPQ_DEBUG=1 make run
# → logs.SetLevelFor("interest", Debug)
```

Name is **`interest`** because of `WithName("interest")`. `SetLevelFor("vpq", …)` silently does nothing useful here — classic “I turned on debug and nothing happened” trap.

---

## 7. Runnable vs “Listen in main”

Motors API implements `cf.Runnable`. `fw.RunWithSignals` cancels a context on SIGTERM; `Run` shuts down the HTTP server; then framework `Shutdown` runs reverse Init order (unsubscribe logs, close pools, …).

If you `http.ListenAndServe` in `main` beside the framework, you re-implement half of that poorly. The demo refuses that shortcut so copy-paste into a real service stays honest.

---

## 8. Logging pattern (AGENTS.md — not optional)

Every component:

1. `GetDependencies` includes `logs`.
2. `Init` → `OnReconfigureFor(Name(), …)` (or `OnReconfigure` when name is default).
3. `Shutdown` → `Unsubscribe`.
4. Cache `*slog.Logger` on the struct — do not call `logs.Logger()` on every request.

Motors API does this. VPQ does this internally. The interest **business** log line (`API.InterestHandler`, wired as the `WithHandler` in `main`) also subscribes under the `"interest"` name so `SetLevelFor("interest")` applies to “follow up this vehicle” lines too.

Bare `slog.Default()` in a component after Init is a bug waiting for “why don’t my JSON logs show up in prod.”

---

## 9. Process shapes (same binary, different shapes)

| Shape | Initialized graph | HTTP | Migrate-on-init |
|---|---|---|---|
| `serve` (default) | full chassis | yes | **yes** (local demo) |
| `--postgresql.job=migrate` | core + postgres (its closure) | no | no |
| `--demoapp.job=seed\|doctor\|price` | app closure = core + whole data plane + app | no | no |

There are no subcommands: every shape is a job flag declared by the owning
component (the app declares `--demoapp.job` with tasks `seed`/`doctor`/`price`;
postgres declares `--postgresql.job` with task `migrate`). `RunWithSignals` asks
configuration (`cf.JobSource`) whether a job flag is set; when one is, it
initializes the **target's dependency closure** — the target's plane and
everything below it — runs the task, tears down, and exits. Nothing outside the
closure initializes, and no Runnables start (the catalog-summary refresher and
the interest VPQ workers do not wake for `seed`/`doctor`/`price`). The flag is
declared by each module on its own configuration source, so configuration
parses/validates the value like any other knob (jobs are CLI-only — the value
never flows from env or file). `price` takes positional args after the flag:
`--demoapp.job=price get <uuid>` / `set <uuid> <cents>`.

---

## 10. Failure drills (learn by breaking)

Try these once:

1. **`make down` then `make run`** — Init should fail talking to Postgres. Read the error; it should name the dependency.
2. **Kill Valkey while serve runs** — `:9090/readyz` fails; `:8081/livez` may still be ok; price GET may still work from Postgres.
3. **Force a dirty migrate** (advanced) — confirm we do not auto-force; fix with operator `migrate force`.
4. **POST interest on Porsche ×3, Corolla ×1** — log order should prefer Porsche.

---

## 11. What this demo deliberately does *not* do

- Real VIN validation, payments, authn/z
- GORM / heavy ORMs
- Helm / Terraform
- Secrets manager module
- gRPC
- Treating VPQ like Sidekiq

Those omissions are features of the teaching surface. Auth belongs in a real
service (e.g. auth-api), not this lot.

---

## 12. If you only remember three things

1. **Ops chassis vs queries** — framework vs `store`.
2. **Migrate Job in prod; migrate-on-init is a local convenience.**
3. **Component `Name()` is how logs levels and `GetByName` work** — here, `"interest"`.

Now go back to [`../README.md`](../README.md) and run `make curl-demo`. The Porsche is waiting.

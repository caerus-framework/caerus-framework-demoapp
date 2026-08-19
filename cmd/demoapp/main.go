// Command demoapp is the Caerus Motors reference binary.
//
// main.go is the golden-path wiring file: declare FrameworkOptions
// (always-on core seeds + chassis + the Motors app class) and RunWithSignals.
// Configuration sources are owned by their components; the framework absorbs
// argv. Product machinery lives under the app as Subcomponents (interest VPQ,
// catalog-summary refresher) — main does not declare those siblings.
//
// Process shapes:
//
//	serve (default)          — HTTP API, interest VPQ, catalog-summary, signals
//	--postgresql.job=migrate — Init postgres + closure → Migrate → exit
//	--demoapp.job=seed       — Init app closure → seed fleet → exit
//	--demoapp.job=doctor     — Init app closure → ping deps → exit
//	--demoapp.job=price      — Init app closure → get|set price → exit
//	                           (positional: get <uuid> | set <uuid> <cents>)
//
// Production multi-replica: run `--postgresql.job=migrate` as a Job, then
// serve without migrate-on-init. This binary keeps migrate-on-init ON so
// `make run` stays one command locally.
package main

import (
	"context"
	"fmt"
	"os"
	"time"

	cf "github.com/caerus-framework/caerus-framework"
	cf_http "github.com/caerus-framework/caerus-framework-http"
	cf_postgres "github.com/caerus-framework/caerus-framework-postgresql"
	cf_valkey "github.com/caerus-framework/caerus-framework-valkey"

	"github.com/caerus-framework/caerus-framework-demoapp/internal/app"
	"github.com/caerus-framework/caerus-framework-demoapp/internal/dbmigrate"
)

func main() {
	// Chassis (postgres, valkey, http) + Motors app. The app constructs interest
	// VPQ and catalog-summary in New and exposes them via Subcomponents(); the
	// framework flattens them into the registry before Init. HTTP listen is
	// cf_http.Run, not the app.
	fw := cf.New(&cf.FrameworkOptions{
		Logs: &cf.LogsSettings{
			Format:       "json",
			Level:        "info",
			ConfigSource: "logs",
		},
		Observability: &cf.ObservabilitySettings{
			Bind:         ":9090",
			ConfigSource: "observability",
		},
		Components: []cf.CaerusComponent{
			cf_postgres.New(
				cf_postgres.WithConfigSource("postgresql", "config/postgresql.json",
					cf_postgres.WithSourceEnvPrefix("POSTGRES_")),
				cf_postgres.WithEmbeddedMigrations(dbmigrate.Migrations, "migrations"),
				// LOCAL / single-replica only. Multi-replica prod: drop this and
				// run `--postgresql.job=migrate` as a K8s Job.
				cf_postgres.WithMigrateOnInit(),
			),
			cf_valkey.New(
				cf_valkey.WithConfigSource("valkey", "config/valkey.json"),
				cf_valkey.WithKeyPrefix("demo:"),
			),
			cf_http.New(
				cf_http.WithConfigSource("http", "config/http.json"),
				cf_http.WithWaitForHealth(30*time.Second),
			),
			app.New(app.Options{}),
		},
	})

	if err := fw.RunWithSignals(context.Background(), cf.WithShutdownTimeout(15*time.Second)); err != nil {
		exitErr(err)
	}
}

func exitErr(err error) {
	fmt.Fprintf(os.Stderr, "demoapp: %v\n", err)
	os.Exit(1)
}

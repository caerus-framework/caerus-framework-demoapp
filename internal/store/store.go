// Package store is the Postgres access layer for Caerus Motors.
//
// We intentionally use raw pgx (not GORM). The framework owns the *pool*
// lifecycle; this package owns SQL. That split is the whole point of
// POSTGRES-HEAVY.md: ops chassis vs query engine.
package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	cf_postgres "github.com/caerus-framework/caerus-framework-postgresql"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrNotFound means the row isn't there — HTTP layer maps this to 404.
var ErrNotFound = errors.New("store: not found")

// Lot is a physical (or fictional) place cars sit.
type Lot struct {
	ID        uuid.UUID `json:"id"`
	Name      string    `json:"name"`
	Address   string    `json:"address"`
	CreatedAt time.Time `json:"created_at"`
}

// Vehicle is a car on the books. VIN values in the demo are DEMO-VIN-* strings,
// not ISO-3779 identifiers — we are teaching wiring, not DMS compliance.
type Vehicle struct {
	ID        uuid.UUID  `json:"id"`
	VIN       string     `json:"vin"`
	Make      string     `json:"make"`
	Model     string     `json:"model"`
	Year      int        `json:"year"`
	Color     string     `json:"color"`
	LotID     *uuid.UUID `json:"lot_id,omitempty"`
	LotName   string     `json:"lot_name,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
	// Price fields filled when we JOIN prices (list endpoints).
	AmountCents *int64  `json:"amount_cents,omitempty"`
	Currency    *string `json:"currency,omitempty"`
}

// Price is the asking price for one vehicle. This is what Valkey caches.
type Price struct {
	VehicleID   uuid.UUID `json:"vehicle_id"`
	AmountCents int64     `json:"amount_cents"`
	Currency    string    `json:"currency"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// Store wraps the framework postgres component. The pool is owned by
// cf_postgres — we never Close it, and we never snapshot Pool() at Init
// (reload swaps the live pool).
type Store struct {
	pg *cf_postgres.CFPostgres
}

// New binds to the postgres component. Call Pool() on every query.
func New(pg *cf_postgres.CFPostgres) *Store {
	return &Store{pg: pg}
}

func (s *Store) pool() *pgxpool.Pool {
	if s == nil || s.pg == nil {
		return nil
	}
	return s.pg.Pool()
}

// ListLots returns every lot, oldest first (stable for demos/screenshots).
func (s *Store) ListLots(ctx context.Context) ([]Lot, error) {
	rows, err := s.pool().Query(ctx, `
		SELECT id, name, address, created_at
		FROM lots
		ORDER BY created_at ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Lot
	for rows.Next() {
		var l Lot
		if err := rows.Scan(&l.ID, &l.Name, &l.Address, &l.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// CreateLot inserts a lot. Name must be unique (DB enforces).
func (s *Store) CreateLot(ctx context.Context, name, address string) (Lot, error) {
	var l Lot
	err := s.pool().QueryRow(ctx, `
		INSERT INTO lots (name, address)
		VALUES ($1, $2)
		RETURNING id, name, address, created_at`, name, address).
		Scan(&l.ID, &l.Name, &l.Address, &l.CreatedAt)
	return l, err
}

// ListVehicles returns cars with optional lot name and price (LEFT JOINs).
func (s *Store) ListVehicles(ctx context.Context) ([]Vehicle, error) {
	rows, err := s.pool().Query(ctx, `
		SELECT v.id, v.vin, v.make, v.model, v.year, v.color, v.lot_id, v.created_at,
		       COALESCE(l.name, ''), p.amount_cents, p.currency
		FROM vehicles v
		LEFT JOIN lots l ON l.id = v.lot_id
		LEFT JOIN prices p ON p.vehicle_id = v.id
		ORDER BY v.created_at ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Vehicle
	for rows.Next() {
		var v Vehicle
		var amount *int64
		var currency *string
		if err := rows.Scan(&v.ID, &v.VIN, &v.Make, &v.Model, &v.Year, &v.Color, &v.LotID, &v.CreatedAt,
			&v.LotName, &amount, &currency); err != nil {
			return nil, err
		}
		v.AmountCents = amount
		v.Currency = currency
		out = append(out, v)
	}
	return out, rows.Err()
}

// CreateVehicle inserts a vehicle. lotID may be nil (unassigned).
func (s *Store) CreateVehicle(ctx context.Context, vin, make, model string, year int, color string, lotID *uuid.UUID) (Vehicle, error) {
	var v Vehicle
	err := s.pool().QueryRow(ctx, `
		INSERT INTO vehicles (vin, make, model, year, color, lot_id)
		VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING id, vin, make, model, year, color, lot_id, created_at`,
		vin, make, model, year, color, lotID).
		Scan(&v.ID, &v.VIN, &v.Make, &v.Model, &v.Year, &v.Color, &v.LotID, &v.CreatedAt)
	return v, err
}

// GetPrice loads asking price from Postgres (source of truth).
func (s *Store) GetPrice(ctx context.Context, vehicleID uuid.UUID) (Price, error) {
	var p Price
	err := s.pool().QueryRow(ctx, `
		SELECT vehicle_id, amount_cents, currency, updated_at
		FROM prices WHERE vehicle_id = $1`, vehicleID).
		Scan(&p.VehicleID, &p.AmountCents, &p.Currency, &p.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Price{}, ErrNotFound
	}
	return p, err
}

// UpsertPrice sets asking price and bumps updated_at. Callers should refresh Valkey after this.
func (s *Store) UpsertPrice(ctx context.Context, vehicleID uuid.UUID, amountCents int64, currency string) (Price, error) {
	if currency == "" {
		currency = "USD"
	}
	var p Price
	err := s.pool().QueryRow(ctx, `
		INSERT INTO prices (vehicle_id, amount_cents, currency, updated_at)
		VALUES ($1, $2, $3, now())
		ON CONFLICT (vehicle_id) DO UPDATE
		SET amount_cents = EXCLUDED.amount_cents,
		    currency = EXCLUDED.currency,
		    updated_at = now()
		RETURNING vehicle_id, amount_cents, currency, updated_at`,
		vehicleID, amountCents, currency).
		Scan(&p.VehicleID, &p.AmountCents, &p.Currency, &p.UpdatedAt)
	if err != nil {
		return Price{}, fmt.Errorf("upsert price: %w", err)
	}
	return p, nil
}

// VehicleExists is a cheap check before enqueueing interest heat.
func (s *Store) VehicleExists(ctx context.Context, id uuid.UUID) (bool, error) {
	var n int
	err := s.pool().QueryRow(ctx, `SELECT 1 FROM vehicles WHERE id = $1`, id).Scan(&n)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

package store

import (
	"context"
	"fmt"
)

// seedCar is one row of the demo fleet. Prices are in cents so we never teach
// float money by accident.
type seedCar struct {
	vin, make, model, color, lot string
	year                         int
	amountCents                  int64
}

// SeedLotsVehiclesPrices is idempotent: re-running `demoapp seed` must not
// duplicate rows. We key lots by name and vehicles by VIN.
//
// Why these cars? Mix of EV / ICE / truck / sports so screenshots look alive,
// and a Porsche so the lot doesn't look like a boring mid-market catalog.
func (s *Store) SeedLotsVehiclesPrices(ctx context.Context) error {
	lots := []struct{ name, address string }{
		{"downtown", "100 Market St"},
		{"airport", "1 Terminal Rd"},
		{"warehouse", "900 Industrial Blvd"},
	}
	lotIDs := map[string]string{}
	for _, l := range lots {
		var id string
		err := s.pool.QueryRow(ctx, `
			INSERT INTO lots (name, address) VALUES ($1, $2)
			ON CONFLICT (name) DO UPDATE SET address = EXCLUDED.address
			RETURNING id::text`, l.name, l.address).Scan(&id)
		if err != nil {
			return fmt.Errorf("seed lot %s: %w", l.name, err)
		}
		lotIDs[l.name] = id
	}

	// amountCents is integer cents (never float dollars). 4_299_000 = $42,990.00
	cars := []seedCar{
		{"DEMO-VIN-001", "Volvo", "EX30", "Cloud Blue", "downtown", 2024, 4_299_000},
		{"DEMO-VIN-002", "Toyota", "Corolla", "Silver", "airport", 2019, 1_599_000},
		{"DEMO-VIN-003", "Ford", "F-150", "Oxford White", "warehouse", 2021, 3_899_000},
		{"DEMO-VIN-004", "Tesla", "Model 3", "Midnight Silver", "downtown", 2022, 3_599_000},
		{"DEMO-VIN-005", "Porsche", "911 Carrera", "Guards Red", "downtown", 2023, 12_990_000},
	}

	for _, c := range cars {
		lotID := lotIDs[c.lot]
		var vehicleID string
		err := s.pool.QueryRow(ctx, `
			INSERT INTO vehicles (vin, make, model, year, color, lot_id)
			VALUES ($1, $2, $3, $4, $5, $6::uuid)
			ON CONFLICT (vin) DO UPDATE
			SET make = EXCLUDED.make, model = EXCLUDED.model, year = EXCLUDED.year,
			    color = EXCLUDED.color, lot_id = EXCLUDED.lot_id
			RETURNING id::text`,
			c.vin, c.make, c.model, c.year, c.color, lotID).Scan(&vehicleID)
		if err != nil {
			return fmt.Errorf("seed vehicle %s: %w", c.vin, err)
		}
		_, err = s.pool.Exec(ctx, `
			INSERT INTO prices (vehicle_id, amount_cents, currency, updated_at)
			VALUES ($1::uuid, $2, 'USD', now())
			ON CONFLICT (vehicle_id) DO UPDATE
			SET amount_cents = EXCLUDED.amount_cents, updated_at = now()`,
			vehicleID, c.amountCents)
		if err != nil {
			return fmt.Errorf("seed price %s: %w", c.vin, err)
		}
	}
	return nil
}

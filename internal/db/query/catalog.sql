-- Catalog summary queries (read-side, sqlc-generated). These feed the
-- catalog-summary Refresher's derived cache: counts are cheap to recompute and
-- only one replica needs to do it (patterns.Mutex in internal/catalogsummary).

-- name: VehiclesByMake :many
SELECT make, COUNT(*) AS vehicle_count
FROM vehicles
GROUP BY make
ORDER BY vehicle_count DESC, make ASC;

-- name: VehicleCount :one
SELECT COUNT(*) AS total
FROM vehicles;

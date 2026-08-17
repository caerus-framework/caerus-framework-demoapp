#!/usr/bin/env bash
# Smoke the Motors API the way a hurried human would — list cars, hit cache,
# poke interest heat a few times. Requires `make run` (or equivalent) already up.
set -euo pipefail

API="${DEMOAPP_API:-http://127.0.0.1:8081}"
OBS="${DEMOAPP_OBS:-http://127.0.0.1:9090}"

echo "== probes (observability owns readiness; API only has /livez) =="
curl -fsS "$API/livez"
echo
curl -fsS "$OBS/readyz" | head -c 400 || true
echo
echo

echo "== lots =="
curl -fsS "$API/v1/lots" | python3 -m json.tool 2>/dev/null || curl -fsS "$API/v1/lots"
echo

echo "== vehicles (look for DEMO-VIN-005 / Porsche) =="
VEHICLES="$(curl -fsS "$API/v1/vehicles")"
echo "$VEHICLES" | python3 -m json.tool 2>/dev/null || echo "$VEHICLES"
echo

# Prefer python for UUID extract so we don't depend on jq.
PORSCHE_ID="$(echo "$VEHICLES" | python3 -c '
import json,sys
rows=json.load(sys.stdin)
for v in rows:
    if v.get("vin")=="DEMO-VIN-005" or "Porsche" in (v.get("make") or ""):
        print(v["id"]); raise SystemExit
if rows: print(rows[0]["id"])
' 2>/dev/null || true)"

if [[ -z "${PORSCHE_ID}" ]]; then
  echo "no vehicles — did you run: make migrate && make seed ?" >&2
  exit 1
fi

echo "== price cache-aside for $PORSCHE_ID =="
echo "-- first GET (expect X-Cache: MISS or HIT) --"
curl -fsS -D - "$API/v1/prices/$PORSCHE_ID" -o /tmp/demoapp-price.json | grep -i x-cache || true
python3 -m json.tool </tmp/demoapp-price.json 2>/dev/null || cat /tmp/demoapp-price.json
echo
echo "-- second GET (should HIT if Valkey is up) --"
curl -fsS -D - "$API/v1/prices/$PORSCHE_ID" -o /dev/null | grep -i x-cache || true
echo

echo "== interest heat: bump Porsche thrice, Corolla once =="
COROLLA_ID="$(echo "$VEHICLES" | python3 -c '
import json,sys
rows=json.load(sys.stdin)
for v in rows:
    if v.get("vin")=="DEMO-VIN-002":
        print(v["id"]); raise SystemExit
' 2>/dev/null || true)"

for _ in 1 2 3; do
  curl -fsS -X POST "$API/v1/interest/$PORSCHE_ID" >/dev/null
done
if [[ -n "${COROLLA_ID}" ]]; then
  curl -fsS -X POST "$API/v1/interest/$COROLLA_ID" >/dev/null
fi
echo "queued. Watch serve logs for: interest heat: follow up this vehicle"
echo "Porsche should appear before Corolla (higher weight)."
echo

echo "== derived catalog summary (reconciled into Valkey; API never touches Postgres) =="
curl -fsS "$API/v1/catalog/summary" | python3 -m json.tool 2>/dev/null || echo "(summary not reconciled yet — wait one reconcile interval and re-run)"
echo
echo "Done. Tip: set component_levels.interest=debug in config/logs.json for louder VPQ internals."

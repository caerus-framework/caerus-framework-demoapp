package app

// DemoAppConfig is *our* settings — not postgres, valkey, or HTTP listen.
// HTTP bind and timeouts live on the http chassis (`config/http.json`,
// `--http-bind`, `HTTP_BIND`). This file is price-cache TTL only.
//
// Per-component log levels (e.g. interest VPQ debug) live on the logs
// source: `config/logs.json` → `component_levels` keyed by Name().
//
// Layering (later wins): config/demoapp.json → DEMOAPP_* env → flags.
type DemoAppConfig struct {
	PriceCacheTTLSec int `json:"price_cache_ttl_sec" yaml:"price_cache_ttl_sec" env:"PRICE_CACHE_TTL_SEC"`
}

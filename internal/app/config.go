package app

// DemoAppConfig is *our* settings — not postgres/valkey. Loaded as a configuration
// source named "demoapp" so you can hot-reload http_addr in theory (the API
// process does not reconnect the listener on reload in v1; file is still the
// single place operators look).
//
// Layering (later wins): config/demoapp.json → DEMOAPP_* env → --http-addr /
// --vpq-debug flags (ParseFlags). See CONFIG-OVERHAUL.md / AGENTS.md.
type DemoAppConfig struct {
	HTTPAddr         string `json:"http_addr" yaml:"http_addr" env:"HTTP_ADDR" flag:"http-addr"`
	PriceCacheTTLSec int    `json:"price_cache_ttl_sec" yaml:"price_cache_ttl_sec" env:"PRICE_CACHE_TTL_SEC"`
	// VPQDebug asks logs.SetLevelFor("interest", Debug) at serve time — the
	// component Name() is "interest" (WithName), not the default "vpq". Useful
	// when interest-heat looks quiet and you need recover/process lines.
	// Accepts file / DEMOAPP_VPQ_DEBUG / --vpq-debug.
	VPQDebug bool `json:"vpq_debug" yaml:"vpq_debug" env:"VPQ_DEBUG" flag:"vpq-debug"`
}

package app

// DemoAppConfig is *our* settings — not postgres, valkey, or HTTP listen.
// HTTP address and timeouts live on the http chassis (`config/http.json`,
// `--http-address`, `HTTP_ADDRESS`). This file is price-cache TTL and
// `--vpq-debug` only.
//
// Layering (later wins): config/demoapp.json → DEMOAPP_* env → flags.
type DemoAppConfig struct {
	PriceCacheTTLSec int `json:"price_cache_ttl_sec" yaml:"price_cache_ttl_sec" env:"PRICE_CACHE_TTL_SEC"`
	// VPQDebug asks logs.SetLevelFor("interest", Debug) at serve time — the
	// component Name() is "interest" (WithName), not the default "vpq". Useful
	// when interest-heat looks quiet and you need recover/process lines.
	// Accepts file / DEMOAPP_VPQ_DEBUG / --vpq-debug.
	VPQDebug bool `json:"vpq_debug" yaml:"vpq_debug" env:"VPQ_DEBUG" flag:"vpq-debug"`
}

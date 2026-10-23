package adapter

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/caddyserver/caddy/v2/caddyconfig"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/caddyconfig/httpcaddyfile"
)

// The global options that produce a tls app, as in the real Caddyfile.
const caddyfileBody = `{
	admin off
	on_demand_tls {
		ask http://127.0.0.1:9000/check
	}
}

https:// {
	tls {
		on_demand
	}
	respond "parked"
}
`

func decode(t *testing.T, config []byte) map[string]any {
	t.Helper()
	var decoded map[string]any
	if err := json.Unmarshal(config, &decoded); err != nil {
		t.Fatalf("decoding %s: %v", config, err)
	}
	return decoded
}

func TestAdapterIsRegistered(t *testing.T) {
	if _, ok := caddyconfig.GetAdapter("parking").(Adapter); !ok {
		t.Fatal(`no "parking" adapter registered`)
	}
}

// The output is the Caddyfile adapter's, with the cache capacity set and
// nothing else changed.
func TestAdaptSetsOnlyTheCacheCapacity(t *testing.T) {
	t.Setenv("CERT_CACHE_CAPACITY", "")
	options := map[string]any{"filename": "Caddyfile"}

	got, _, err := Adapter{}.Adapt([]byte(caddyfileBody), options)
	if err != nil {
		t.Fatalf("Adapt: %v", err)
	}
	plain, _, err := caddyfile.Adapter{ServerType: httpcaddyfile.ServerType{}}.Adapt([]byte(caddyfileBody), options)
	if err != nil {
		t.Fatalf("Caddyfile adapter: %v", err)
	}

	adapted := decode(t, got)
	tls := adapted["apps"].(map[string]any)["tls"].(map[string]any)
	cache, ok := tls["cache"].(map[string]any)
	if !ok {
		t.Fatalf("no apps.tls.cache in %s", got)
	}
	if capacity := cache["capacity"]; capacity != float64(CacheCapacity) {
		t.Errorf("apps.tls.cache.capacity = %v, want %d", capacity, CacheCapacity)
	}

	delete(tls, "cache")
	if want := decode(t, plain); !reflect.DeepEqual(adapted, want) {
		t.Errorf("Adapt changed more than the cache:\n got %v\nwant %v", adapted, want)
	}
}

func TestCacheCapacityFromTheEnvironment(t *testing.T) {
	t.Setenv("CERT_CACHE_CAPACITY", " 250000 ")
	got, _, err := Adapter{}.Adapt([]byte(caddyfileBody), nil)
	if err != nil {
		t.Fatalf("Adapt: %v", err)
	}
	tls := decode(t, got)["apps"].(map[string]any)["tls"].(map[string]any)
	if capacity := tls["cache"].(map[string]any)["capacity"]; capacity != float64(250_000) {
		t.Errorf("apps.tls.cache.capacity = %v, want 250000", capacity)
	}
}

func TestInvalidCacheCapacityIsRefused(t *testing.T) {
	for _, value := range []string{"lots", "0", "-5", "1e6"} {
		t.Setenv("CERT_CACHE_CAPACITY", value)
		if _, _, err := (Adapter{}).Adapt([]byte(caddyfileBody), nil); err == nil {
			t.Errorf("Adapt accepted CERT_CACHE_CAPACITY=%q", value)
		}
	}
}

func TestAdaptKeepsCaddyfileErrors(t *testing.T) {
	if _, _, err := (Adapter{}).Adapt([]byte("{\n\tnot_an_option\n}\n"), nil); err == nil {
		t.Fatal("Adapt accepted an invalid Caddyfile")
	}
}

func TestSetCacheCapacityWithoutATLSApp(t *testing.T) {
	got, err := setCacheCapacity([]byte(`{"admin":{"disabled":true}}`), 7)
	if err != nil {
		t.Fatalf("setCacheCapacity: %v", err)
	}

	want := map[string]any{
		"admin": map[string]any{"disabled": true},
		"apps":  map[string]any{"tls": map[string]any{"cache": map[string]any{"capacity": float64(7)}}},
	}
	if decoded := decode(t, got); !reflect.DeepEqual(decoded, want) {
		t.Errorf("got %v, want %v", decoded, want)
	}
}

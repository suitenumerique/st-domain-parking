// Package adapter registers the "parking" config adapter: the Caddyfile
// adapter, plus the settings the Caddyfile has no syntax for.
//
// The only one is the certificate cache capacity. Caddy keeps at most 10,000
// certificates in memory by default and evicts at random beyond that, so a
// handshake for an evicted certificate reloads it from storage. Every parked
// domain needs two certificates, www and apex, and the estate is past that
// limit. The capacity is only settable in JSON (`apps.tls.cache.capacity`).
package adapter

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/caddyserver/caddy/v2/caddyconfig"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/caddyconfig/httpcaddyfile"
)

// CacheCapacity is the default number of certificates Caddy keeps in
// memory, a few kB each, so a few hundred MB when full. Only the
// certificates requested since startup are loaded. CERT_CACHE_CAPACITY
// overrides it.
const CacheCapacity = 100_000

func init() {
	caddyconfig.RegisterAdapter("parking", Adapter{})
}

// Adapter adapts a Caddyfile, then sets what the Caddyfile cannot.
type Adapter struct{}

// Adapt implements caddyconfig.Adapter.
func (Adapter) Adapt(body []byte, options map[string]any) ([]byte, []caddyconfig.Warning, error) {
	adapted, warnings, err := caddyfile.Adapter{ServerType: httpcaddyfile.ServerType{}}.Adapt(body, options)
	if err != nil {
		return nil, warnings, err
	}

	capacity, err := cacheCapacity()
	if err != nil {
		return nil, warnings, err
	}
	adapted, err = setCacheCapacity(adapted, capacity)
	if err != nil {
		return nil, warnings, fmt.Errorf("setting the certificate cache capacity: %w", err)
	}
	return adapted, warnings, nil
}

// cacheCapacity reads CERT_CACHE_CAPACITY, CacheCapacity when unset.
func cacheCapacity() (int, error) {
	raw := strings.TrimSpace(os.Getenv("CERT_CACHE_CAPACITY"))
	if raw == "" {
		return CacheCapacity, nil
	}
	capacity, err := strconv.Atoi(raw)
	if err != nil || capacity < 1 {
		return 0, fmt.Errorf("CERT_CACHE_CAPACITY must be a positive number of certificates, got %q", raw)
	}
	return capacity, nil
}

// setCacheCapacity sets apps.tls.cache.capacity. Everything else is passed
// through as raw JSON.
func setCacheCapacity(config []byte, capacity int) ([]byte, error) {
	var root map[string]json.RawMessage
	if err := json.Unmarshal(config, &root); err != nil {
		return nil, err
	}

	apps := map[string]json.RawMessage{}
	if raw, ok := root["apps"]; ok {
		if err := json.Unmarshal(raw, &apps); err != nil {
			return nil, err
		}
	}

	tls := map[string]json.RawMessage{}
	if raw, ok := apps["tls"]; ok {
		if err := json.Unmarshal(raw, &tls); err != nil {
			return nil, err
		}
	}

	var err error
	if tls["cache"], err = json.Marshal(map[string]int{"capacity": capacity}); err != nil {
		return nil, err
	}
	if apps["tls"], err = json.Marshal(tls); err != nil {
		return nil, err
	}
	if root["apps"], err = json.Marshal(apps); err != nil {
		return nil, err
	}
	return json.Marshal(root)
}

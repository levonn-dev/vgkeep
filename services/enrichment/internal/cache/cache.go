// Package cache is the enrichment service's Valkey surface: the 24h
// search cache and short product cache, storing marshaled response
// bodies. FAIL-OPEN DECISIONS BELONG TO CALLERS: errors return
// verbatim (handlers treat as a miss); misses are nil, not errors.
// No per-operation timeouts; those belong to the client options.
package cache

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/levonn-dev/vgkeep/libs/go/valkeykit"
)

// Cache wraps the service's Valkey client.
type Cache struct {
	rdb *redis.Client
}

// New builds a Cache over an already-connected client.
func New(rdb *redis.Client) *Cache {
	return &Cache{rdb: rdb}
}

// searchKey hashes the (caller-normalized) query so keys stay clean.
// The version segment bumps on schema change; old entries age out via TTL.
func searchKey(kind, q string) string {
	sum := sha256.Sum256([]byte(q))
	return "search:v4:" + kind + ":" + hex.EncodeToString(sum[:])
}

func productKey(id string) string { return "product:v1:" + id }

// GetSearch returns the cached response body for a query, or nil.
func (c *Cache) GetSearch(ctx context.Context, kind, q string) ([]byte, error) {
	return valkeykit.GetBytes(ctx, c.rdb, searchKey(kind, q), "cache: get search")
}

// PutSearch caches a search response body.
func (c *Cache) PutSearch(ctx context.Context, kind, q string, body []byte, ttl time.Duration) error {
	return valkeykit.PutBytes(ctx, c.rdb, searchKey(kind, q), body, ttl, "cache: put search")
}

// GetProduct returns the cached product body, or nil.
func (c *Cache) GetProduct(ctx context.Context, id string) ([]byte, error) {
	return valkeykit.GetBytes(ctx, c.rdb, productKey(id), "cache: get product")
}

// PutProduct caches a product body.
func (c *Cache) PutProduct(ctx context.Context, id string, body []byte, ttl time.Duration) error {
	return valkeykit.PutBytes(ctx, c.rdb, productKey(id), body, ttl, "cache: put product")
}

// InvalidateProduct drops a product's cache entry (admin mapping
// corrections must be visible immediately, not after the TTL).
func (c *Cache) InvalidateProduct(ctx context.Context, id string) error {
	if err := c.rdb.Del(ctx, productKey(id)).Err(); err != nil {
		return fmt.Errorf("cache: invalidate product: %w", err)
	}
	return nil
}

// platformsKey is the single wholesale key for the platform catalog
// (no per-query variance, unlike the hashed search keys).
const platformsKey = "platforms:v1"

// GetPlatforms returns the cached platform-catalog body, or nil.
func (c *Cache) GetPlatforms(ctx context.Context) ([]byte, error) {
	return valkeykit.GetBytes(ctx, c.rdb, platformsKey, "cache: get platforms")
}

// PutPlatforms caches the platform-catalog body.
func (c *Cache) PutPlatforms(ctx context.Context, body []byte, ttl time.Duration) error {
	return valkeykit.PutBytes(ctx, c.rdb, platformsKey, body, ttl, "cache: put platforms")
}

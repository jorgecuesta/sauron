package storage

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"
)

// Cache provides optional Redis caching
// The vaults beneath the tower
type Cache struct {
	client *redis.Client // nil if disabled
	logger *zap.Logger
}

// NewCache creates a new cache instance
// If URI is empty, cache is disabled (client will be nil)
func NewCache(uri string, logger *zap.Logger) *Cache {
	if uri == "" {
		logger.Info("Redis cache disabled")
		return &Cache{
			client: nil,
			logger: logger,
		}
	}

	// Parse Redis URI
	opt, err := redis.ParseURL(uri)
	if err != nil {
		logger.Error("Failed to parse Redis URI, cache disabled", zap.Error(err))
		return &Cache{
			client: nil,
			logger: logger,
		}
	}

	client := redis.NewClient(opt)

	// Test connection
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := client.Ping(ctx).Err(); err != nil {
		logger.Warn("Redis unavailable, running without cache", zap.Error(err))
		return &Cache{
			client: nil,
			logger: logger,
		}
	}

	logger.Info("Redis cache enabled", zap.String("addr", opt.Addr))
	return &Cache{
		client: client,
		logger: logger,
	}
}

// SetHeight caches a height value with TTL
func (c *Cache) SetHeight(ctx context.Context, network, node, endpointType string, height int64, ttl time.Duration) {
	if c.client == nil {
		return
	}

	key := fmt.Sprintf("height:%s:%s:%s", network, node, endpointType)
	if err := c.client.Set(ctx, key, height, ttl).Err(); err != nil {
		c.logger.Warn("Failed to set cache", zap.String("key", key), zap.Error(err))
	}
}

// GetHeight retrieves a cached height value
func (c *Cache) GetHeight(ctx context.Context, network, node, endpointType string) (int64, bool) {
	if c.client == nil {
		return 0, false
	}

	key := fmt.Sprintf("height:%s:%s:%s", network, node, endpointType)
	val, err := c.client.Get(ctx, key).Int64()
	if err != nil {
		if err != redis.Nil {
			c.logger.Warn("Failed to get cache", zap.String("key", key), zap.Error(err))
		}
		return 0, false
	}

	return val, true
}

// SetLatency caches a latency value
func (c *Cache) SetLatency(ctx context.Context, network, node, endpointType string, latency time.Duration, ttl time.Duration) {
	if c.client == nil {
		return
	}

	key := fmt.Sprintf("latency:%s:%s:%s", network, node, endpointType)
	if err := c.client.Set(ctx, key, latency.Milliseconds(), ttl).Err(); err != nil {
		c.logger.Warn("Failed to set latency cache", zap.String("key", key), zap.Error(err))
	}
}

// Close closes the Redis connection
func (c *Cache) Close() error {
	if c.client == nil {
		return nil
	}
	return c.client.Close()
}

// IsEnabled returns whether caching is enabled
func (c *Cache) IsEnabled() bool {
	return c.client != nil
}

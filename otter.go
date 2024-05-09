package gubernator

import (
	"fmt"

	"github.com/mailgun/holster/v4/setter"
	"github.com/maypok86/otter"
)

type OtterCache struct {
	cache otter.Cache[string, *CacheItem]
}

// NewOtterCache returns a new cache backed by otter. If size is 0, then
// the cache is created with a default cache size.
func NewOtterCache(size int) (*OtterCache, error) {
	setter.SetDefault(&size, 150_000)
	b, err := otter.NewBuilder[string, *CacheItem](size)
	if err != nil {
		return nil, fmt.Errorf("during otter.NewBuilder(): %w", err)
	}

	b.DeletionListener(func(key string, value *CacheItem, cause otter.DeletionCause) {
		if cause == otter.Size {
			metricCacheUnexpiredEvictions.Add(1)
		}
	})

	cache, err := b.Build()
	if err != nil {
		return nil, fmt.Errorf("during otter.Builder.Build(): %w", err)
	}
	return &OtterCache{
		cache: cache,
	}, nil
}

// Add adds a new CacheItem to the cache. The key must be provided via CacheItem.Key
// returns true if the item was added to the cache; false if the item was too large
// for the cache.
func (o *OtterCache) Add(item *CacheItem) bool {
	return o.cache.Set(item.Key, item)
}

// GetItem returns an item in the cache that corresponds to the provided key
func (o *OtterCache) GetItem(key string) (*CacheItem, bool) {
	item, ok := o.cache.Get(key)
	if !ok {
		metricCacheAccess.WithLabelValues("miss").Add(1)
		return nil, false
	}

	if item.IsExpired() {
		metricCacheAccess.WithLabelValues("miss").Add(1)
		// If the item is expired, just return `nil`
		//
		// We avoid the explicit deletion of the expired item to avoid acquiring a mutex lock in otter.
		// Explicit deletions in otter require a mutex, which can cause performance bottlenecks
		// under high concurrency scenarios. By allowing the item to be evicted naturally by
		// otter's eviction mechanism, we avoid impacting performance under high concurrency.
		return nil, false
	}
	metricCacheAccess.WithLabelValues("hit").Add(1)
	return item, true
}

// UpdateExpiration will update an item in the cache with a new expiration date.
// returns true if the item exists in the cache and was updated.
func (o *OtterCache) UpdateExpiration(key string, expireAt int64) bool {
	item, ok := o.cache.Get(key)
	if !ok {
		return false
	}

	item.ExpireAt = expireAt
	return true
}

// Each returns a channel which the call can use to iterate through
// all the items in the cache.
func (o *OtterCache) Each() chan *CacheItem {
	ch := make(chan *CacheItem)

	go func() {
		o.cache.Range(func(_ string, v *CacheItem) bool {
			ch <- v
			return true
		})
		close(ch)
	}()
	return ch
}

// Remove explicitly removes and item from the cache.
// NOTE: A deletion call to otter requires a mutex to preform,
// if possible, avoid preforming explicit removal from the cache.
// Instead, prefer the item to be evicted naturally.
func (o *OtterCache) Remove(key string) {
	o.cache.Delete(key)
}

// Size return the current number of items in the cache
func (o *OtterCache) Size() int64 {
	return int64(o.cache.Size())
}

// Close closes the cache and all associated background processes
func (o *OtterCache) Close() error {
	o.cache.Close()
	return nil
}

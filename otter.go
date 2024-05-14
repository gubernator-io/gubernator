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
	// Default is 500k bytes in size
	setter.SetDefault(&size, 500_000)
	b, err := otter.NewBuilder[string, *CacheItem](size)
	if err != nil {
		return nil, fmt.Errorf("during otter.NewBuilder(): %w", err)
	}

	b.DeletionListener(func(key string, value *CacheItem, cause otter.DeletionCause) {
		if cause == otter.Size {
			metricCacheUnexpiredEvictions.Add(1)
		}
	})

	b.Cost(func(key string, value *CacheItem) uint32 {
		// The total size of the CacheItem and Bucket item is 104 bytes.
		// See cache.go:CacheItem definition for details.
		return uint32(104 + len(value.Key))
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
// for the cache or already exists in the cache.
func (o *OtterCache) Add(item *CacheItem) bool {
	return o.cache.SetIfAbsent(item.Key, item)
}

// GetItem returns an item in the cache that corresponds to the provided key
func (o *OtterCache) GetItem(key string) (*CacheItem, bool) {
	item, ok := o.cache.Get(key)
	if !ok {
		metricCacheAccess.WithLabelValues("miss").Add(1)
		return nil, false
	}

	metricCacheAccess.WithLabelValues("hit").Add(1)
	return item, true
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

package gubernator

import (
	"fmt"
	"sync/atomic"

	"github.com/mailgun/holster/v4/setter"
	"github.com/maypok86/otter"
)

type OtterCache struct {
	cache otter.Cache[string, *CacheItem]
	stats CacheStats
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

	o := &OtterCache{}

	b.DeletionListener(func(key string, value *CacheItem, cause otter.DeletionCause) {
		if cause == otter.Size {
			atomic.AddInt64(&o.stats.UnexpiredEvictions, 1)
		}
	})

	b.Cost(func(key string, value *CacheItem) uint32 {
		// The total size of the CacheItem and Bucket item is 104 bytes.
		// See cache.go:CacheItem definition for details.
		return uint32(104 + len(value.Key))
	})

	o.cache, err = b.Build()
	if err != nil {
		return nil, fmt.Errorf("during otter.Builder.Build(): %w", err)
	}
	return o, nil
}

// AddIfNotPresent adds a new CacheItem to the cache. The key must be provided via CacheItem.Key
// returns true if the item was added to the cache; false if the item was too large
// for the cache or if the key already exists in the cache.
func (o *OtterCache) AddIfNotPresent(item *CacheItem) bool {
	return o.cache.SetIfAbsent(item.Key, item)
}

// GetItem returns an item in the cache that corresponds to the provided key
func (o *OtterCache) GetItem(key string) (*CacheItem, bool) {
	item, ok := o.cache.Get(key)
	if !ok {
		atomic.AddInt64(&o.stats.Miss, 1)
		return nil, false
	}

	atomic.AddInt64(&o.stats.Hit, 1)
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

// Stats returns the current cache stats and resets the values to zero
func (o *OtterCache) Stats() CacheStats {
	var result CacheStats
	result.UnexpiredEvictions = atomic.SwapInt64(&o.stats.UnexpiredEvictions, 0)
	result.Miss = atomic.SwapInt64(&o.stats.Miss, 0)
	result.Hit = atomic.SwapInt64(&o.stats.Hit, 0)
	result.Size = int64(o.cache.Size())
	return result
}

// Close closes the cache and all associated background processes
func (o *OtterCache) Close() error {
	o.cache.Close()
	return nil
}

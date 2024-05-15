/*
Modifications Copyright 2023 Mailgun Technologies Inc

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

     http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package gubernator

import (
	"sync"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/mailgun/holster/v4/clock"
)

type Cache interface {
	// Add adds an item, or replaces an item in the cache
	//
	// Deprecated: Gubernator algorithms now use AddIfNotExists.
	// TODO: Remove this method in v3
	Add(item *CacheItem) bool

	// AddIfNotPresent adds the item to the cache if it doesn't already exist.
	// Returns true if the item was added, false if the item already exists.
	AddIfNotPresent(item *CacheItem) bool

	GetItem(key string) (value *CacheItem, ok bool)
	Each() chan *CacheItem
	Remove(key string)
	Size() int64
	Stats() CacheStats
	Close() error
}

// CacheItem is 64 bytes aligned in size
// Since both TokenBucketItem and LeakyBucketItem both 40 bytes in size then a CacheItem with
// the Value attached takes up 64 + 40 = 104 bytes of space. Not counting the size of the key.
type CacheItem struct {
	mutex sync.Mutex  // 8  bytes
	Key   string      // 16 bytes
	Value interface{} // 16 bytes

	// Timestamp when rate limit expires in epoch milliseconds.
	ExpireAt int64 // 8 Bytes
	// Timestamp when the cache should invalidate this rate limit. This is useful when used in conjunction with
	// a persistent store to ensure our node has the most up to date info from the store. Ignored if set to `0`
	// It is set by the persistent store implementation to indicate when the node should query the persistent store
	// for the latest rate limit data.
	InvalidAt int64     // 8 bytes
	Algorithm Algorithm // 4 bytes
	// 4 Bytes of Padding
}

func (item *CacheItem) IsExpired() bool {
	// TODO(thrawn01): Eliminate the need for this mutex lock
	item.mutex.Lock()
	defer item.mutex.Unlock()

	now := MillisecondNow()

	// If the entry is invalidated
	if item.InvalidAt != 0 && item.InvalidAt < now {
		return true
	}

	// If the entry has expired, remove it from the cache
	if item.ExpireAt < now {
		return true
	}

	return false
}

func (item *CacheItem) Copy(from *CacheItem) {
	item.mutex.Lock()
	defer item.mutex.Unlock()

	item.InvalidAt = from.InvalidAt
	item.Algorithm = from.Algorithm
	item.ExpireAt = from.ExpireAt
	item.Value = from.Value
	item.Key = from.Key
}

// MillisecondNow returns unix epoch in milliseconds
func MillisecondNow() int64 {
	return clock.Now().UnixNano() / 1000000
}

type CacheStats struct {
	Size               int64
	Hit                int64
	Miss               int64
	UnexpiredEvictions int64
}

// CacheCollector provides prometheus metrics collector for Cache implementations
// Register only one collector, add one or more caches to this collector.
type CacheCollector struct {
	caches                   []Cache
	metricSize               prometheus.Gauge
	metricAccess             *prometheus.CounterVec
	metricUnexpiredEvictions prometheus.Counter
}

func NewCacheCollector() *CacheCollector {
	return &CacheCollector{
		caches: []Cache{},
		metricSize: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "gubernator_cache_size",
			Help: "The number of items in LRU Cache which holds the rate limits.",
		}),
		metricAccess: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gubernator_cache_access_count",
			Help: "Cache access counts.  Label \"type\" = hit|miss.",
		}, []string{"type"}),
		metricUnexpiredEvictions: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "gubernator_unexpired_evictions_count",
			Help: "Count the number of cache items which were evicted while unexpired.",
		}),
	}
}

var _ prometheus.Collector = &CacheCollector{}

// AddCache adds a Cache object to be tracked by the collector.
func (c *CacheCollector) AddCache(cache Cache) {
	c.caches = append(c.caches, cache)
}

// Describe fetches prometheus metrics to be registered
func (c *CacheCollector) Describe(ch chan<- *prometheus.Desc) {
	c.metricSize.Describe(ch)
	c.metricAccess.Describe(ch)
	c.metricUnexpiredEvictions.Describe(ch)
}

// Collect fetches metric counts and gauges from the cache
func (c *CacheCollector) Collect(ch chan<- prometheus.Metric) {
	stats := c.getStats()
	c.metricSize.Set(float64(stats.Size))
	c.metricSize.Collect(ch)
	c.metricAccess.WithLabelValues("miss").Add(float64(stats.Miss))
	c.metricAccess.WithLabelValues("hit").Add(float64(stats.Hit))
	c.metricAccess.Collect(ch)
	c.metricUnexpiredEvictions.Add(float64(stats.UnexpiredEvictions))
	c.metricUnexpiredEvictions.Collect(ch)
}

func (c *CacheCollector) getStats() CacheStats {
	var total CacheStats
	for _, cache := range c.caches {
		stats := cache.Stats()
		total.Hit += stats.Hit
		total.Miss += stats.Miss
		total.Size += stats.Size
		total.UnexpiredEvictions += stats.UnexpiredEvictions
	}
	return total
}

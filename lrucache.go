/*
Modifications Copyright 2018-2022 Mailgun Technologies Inc

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

     http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.

This work is derived from github.com/golang/groupcache/lru
*/

package gubernator

import (
	"container/list"
	"sync"
	"sync/atomic"

	"github.com/mailgun/holster/v4/clock"
	"github.com/mailgun/holster/v4/setter"
	"github.com/prometheus/client_golang/prometheus"
)

// LRUCache is an LRU cache that supports expiration and is not thread-safe
// Be sure to use a mutex to prevent concurrent method calls.
type LRUCache struct {
	cache     map[string]*list.Element
	ll        *list.List
	mu        sync.Mutex
	stats     CacheStats
	cacheSize int
	cacheLen  int64
}

type CacheStats struct {
	Size               int64
	Hit                int64
	Miss               int64
	UnexpiredEvictions int64
}

var _ Cache = &LRUCache{}

// NewLRUCache creates a new Cache with a maximum size.
func NewLRUCache(maxSize int) *LRUCache {
	setter.SetDefault(&maxSize, 50_000)

	return &LRUCache{
		cache:     make(map[string]*list.Element),
		ll:        list.New(),
		cacheSize: maxSize,
	}
}

// Each is not thread-safe. Each() maintains a goroutine that iterates.
// Other go routines cannot safely access the Cache while iterating.
// It would be safer if this were done using an iterator or delegate pattern
// that doesn't require a goroutine. May need to reassess functional requirements.
func (c *LRUCache) Each() chan *CacheItem {
	out := make(chan *CacheItem)
	go func() {
		for _, ele := range c.cache {
			out <- ele.Value.(*CacheItem)
		}
		close(out)
	}()
	return out
}

// Add adds a value to the cache.
func (c *LRUCache) Add(item *CacheItem) bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	// If the key already exist, set the new value
	if ee, ok := c.cache[item.Key]; ok {
		c.ll.MoveToFront(ee)
		ee.Value = item
		return true
	}

	ele := c.ll.PushFront(item)
	c.cache[item.Key] = ele
	if c.cacheSize != 0 && c.ll.Len() > c.cacheSize {
		c.removeOldest()
	}
	atomic.StoreInt64(&c.cacheLen, int64(c.ll.Len()))
	return false
}

// MillisecondNow returns unix epoch in milliseconds
func MillisecondNow() int64 {
	return clock.Now().UnixNano() / 1000000
}

// GetItem returns the item stored in the cache
func (c *LRUCache) GetItem(key string) (item *CacheItem, ok bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if ele, hit := c.cache[key]; hit {
		entry := ele.Value.(*CacheItem)

		c.stats.Hit++
		c.ll.MoveToFront(ele)
		return entry, true
	}

	c.stats.Miss++
	return
}

// Remove removes the provided key from the cache.
func (c *LRUCache) Remove(key string) {
	if ele, hit := c.cache[key]; hit {
		c.removeElement(ele)
	}
}

// RemoveOldest removes the oldest item from the cache.
func (c *LRUCache) removeOldest() {
	ele := c.ll.Back()
	if ele != nil {
		entry := ele.Value.(*CacheItem)

		if MillisecondNow() < entry.ExpireAt {
			c.stats.UnexpiredEvictions++
		}

		c.removeElement(ele)
	}
}

func (c *LRUCache) removeElement(e *list.Element) {
	c.ll.Remove(e)
	kv := e.Value.(*CacheItem)
	delete(c.cache, kv.Key)
	atomic.StoreInt64(&c.cacheLen, int64(c.ll.Len()))
}

// Size returns the number of items in the cache.
func (c *LRUCache) Size() int64 {
	return atomic.LoadInt64(&c.cacheLen)
}

func (c *LRUCache) Close() error {
	c.cache = nil
	c.ll = nil
	c.cacheLen = 0
	return nil
}

// Stats returns the current status for the cache
func (c *LRUCache) Stats() CacheStats {
	c.mu.Lock()
	defer func() {
		c.stats = CacheStats{}
		c.mu.Unlock()
	}()

	c.stats.Size = atomic.LoadInt64(&c.cacheLen)
	return c.stats
}

// CacheCollector provides prometheus metrics collector for LRUCache.
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

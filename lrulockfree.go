package gubernator

import (
	"container/list"
	"sync/atomic"

	"github.com/mailgun/holster/v4/setter"
	"github.com/prometheus/client_golang/prometheus"
)

// LRULockFree is a lock free, thread safe LRU cache that supports expiration
type LRULockFree struct {
	cache     map[string]*list.Element
	ll        *list.List
	cacheSize int
	cacheLen  int64
}

// LRULockFreeCollector provides prometheus metrics collector for LRULockFree.
// Register only one collector, add one or more caches to this collector.
type LRULockFreeCollector struct {
	caches []Cache
}

var _ Cache = &LRULockFree{}
var _ prometheus.Collector = &LRULockFreeCollector{}

// NewLRULockFree creates a new Cache with a maximum size.
func NewLRULockFree(maxSize int) *LRULockFree {
	setter.SetDefault(&maxSize, 50_000)

	return &LRULockFree{
		cache:     make(map[string]*list.Element),
		ll:        list.New(),
		cacheSize: maxSize,
	}
}

// Each is not thread-safe. Each() maintains a goroutine that iterates.
// Other go routines cannot safely access the Cache while iterating.
// It would be safer if this were done using an iterator or delegate pattern
// that doesn't require a goroutine. May need to reassess functional requirements.
func (c *LRULockFree) Each() chan *CacheItem {
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
func (c *LRULockFree) Add(item *CacheItem) bool {
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

// GetItem returns the item stored in the cache
func (c *LRULockFree) GetItem(key string) (item *CacheItem, ok bool) {
	if ele, hit := c.cache[key]; hit {
		entry := ele.Value.(*CacheItem)

		if entry.IsExpired() {
			c.removeElement(ele)
			metricCacheAccess.WithLabelValues("miss").Add(1)
			return
		}

		metricCacheAccess.WithLabelValues("hit").Add(1)
		c.ll.MoveToFront(ele)
		return entry, true
	}

	metricCacheAccess.WithLabelValues("miss").Add(1)
	return
}

// Remove removes the provided key from the cache.
func (c *LRULockFree) Remove(key string) {
	if ele, hit := c.cache[key]; hit {
		c.removeElement(ele)
	}
}

// RemoveOldest removes the oldest item from the cache.
func (c *LRULockFree) removeOldest() {
	ele := c.ll.Back()
	if ele != nil {
		entry := ele.Value.(*CacheItem)

		if MillisecondNow() < entry.ExpireAt {
			metricCacheUnexpiredEvictions.Add(1)
		}

		c.removeElement(ele)
	}
}

func (c *LRULockFree) removeElement(e *list.Element) {
	c.ll.Remove(e)
	kv := e.Value.(*CacheItem)
	delete(c.cache, kv.Key)
	atomic.StoreInt64(&c.cacheLen, int64(c.ll.Len()))
}

// Size returns the number of items in the cache.
func (c *LRULockFree) Size() int64 {
	return atomic.LoadInt64(&c.cacheLen)
}

// UpdateExpiration updates the expiration time for the key
func (c *LRULockFree) UpdateExpiration(key string, expireAt int64) bool {
	if ele, hit := c.cache[key]; hit {
		entry := ele.Value.(*CacheItem)
		entry.ExpireAt = expireAt
		return true
	}
	return false
}

func (c *LRULockFree) Close() error {
	c.cache = nil
	c.ll = nil
	c.cacheLen = 0
	return nil
}

func NewLRULockFreeCollector() *LRULockFreeCollector {
	return &LRULockFreeCollector{
		caches: []Cache{},
	}
}

// AddCache adds a Cache object to be tracked by the collector.
func (collector *LRULockFreeCollector) AddCache(cache Cache) {
	collector.caches = append(collector.caches, cache)
}

// Describe fetches prometheus metrics to be registered
func (collector *LRULockFreeCollector) Describe(ch chan<- *prometheus.Desc) {
	metricCacheSize.Describe(ch)
	metricCacheAccess.Describe(ch)
	metricCacheUnexpiredEvictions.Describe(ch)
}

// Collect fetches metric counts and gauges from the cache
func (collector *LRULockFreeCollector) Collect(ch chan<- prometheus.Metric) {
	metricCacheSize.Set(collector.getSize())
	metricCacheSize.Collect(ch)
	metricCacheAccess.Collect(ch)
	metricCacheUnexpiredEvictions.Collect(ch)
}

func (collector *LRULockFreeCollector) getSize() float64 {
	var size float64

	for _, cache := range collector.caches {
		size += float64(cache.Size())
	}

	return size
}

/*
Modifications Copyright 2024 Derrick Wippler

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

	"github.com/kapetan-io/tackle/set"
)

// LRUCache is a mutex protected LRU cache that supports expiration and is thread-safe
type LRUCache struct {
	cache     map[string]*list.Element
	ll        *list.List
	mu        sync.Mutex
	stats     CacheStats
	cacheSize int
	cacheLen  int64
}

var _ Cache = &LRUCache{}

// NewLRUCache creates a new Cache with a maximum size.
func NewLRUCache(maxSize int) *LRUCache {
	set.Default(&maxSize, 50_000)

	return &LRUCache{
		cache:     make(map[string]*list.Element),
		ll:        list.New(),
		cacheSize: maxSize,
	}
}

// Each maintains a goroutine that iterates over every item in the cache.
// Other go routines operating on this cache will block until all items
// are read from the returned channel.
func (c *LRUCache) Each() chan *CacheItem {
	out := make(chan *CacheItem)
	go func() {
		c.mu.Lock()
		defer c.mu.Unlock()

		for _, ele := range c.cache {
			out <- ele.Value.(*CacheItem)
		}
		close(out)
	}()
	return out
}

// AddIfNotPresent adds the item to the cache if it doesn't already exist.
// Returns true if the item was added, false if the item already exists.
func (c *LRUCache) AddIfNotPresent(item *CacheItem) bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	// If the key already exist, do nothing
	if _, ok := c.cache[item.Key]; ok {
		return false
	}

	ele := c.ll.PushFront(item)
	c.cache[item.Key] = ele
	if c.cacheSize != 0 && c.ll.Len() > c.cacheSize {
		c.removeOldest()
	}
	atomic.StoreInt64(&c.cacheLen, int64(c.ll.Len()))
	return true
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
	c.mu.Lock()
	defer c.mu.Unlock()

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

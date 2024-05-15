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
	"sync/atomic"

	"github.com/mailgun/holster/v4/setter"
)

// LRUCache is an LRU cache that supports expiration and is not thread-safe
// Be sure to use a mutex to prevent concurrent method calls.
//
// Deprecated: Use LRUMutexCache instead. This will be removed in v3
type LRUCache struct {
	cache     map[string]*list.Element
	ll        *list.List
	stats     CacheStats
	cacheSize int
	cacheLen  int64
}

var _ Cache = &LRUCache{}

// NewLRUCache creates a new Cache with a maximum size.
// Deprecated: Use NewLRUMutexCache instead. This will be removed in v3
func NewLRUCache(maxSize int) *LRUCache {
	setter.SetDefault(&maxSize, 50_000)

	return &LRUCache{
		cache:     make(map[string]*list.Element),
		ll:        list.New(),
		cacheSize: maxSize,
	}
}

// Each maintains a goroutine that iterates. Other go routines cannot safely
// access the Cache while iterating. It would be safer if this were done
// using an iterator or delegate pattern that doesn't require a goroutine.
// May need to reassess functional requirements.
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

// AddIfNotPresent adds a value to the cache. returns true if value was added to the cache,
// false if it already exists in the cache
func (c *LRUCache) AddIfNotPresent(item *CacheItem) bool {
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

// Add adds a value to the cache.
// Deprecated: Gubernator algorithms now use AddIfNotExists.
// This method will be removed in the next major version
func (c *LRUCache) Add(item *CacheItem) bool {
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
func (c *LRUCache) GetItem(key string) (item *CacheItem, ok bool) {
	if ele, hit := c.cache[key]; hit {
		entry := ele.Value.(*CacheItem)

		if entry.IsExpired() {
			c.removeElement(ele)
			atomic.AddInt64(&c.stats.Miss, 1)
			return
		}

		atomic.AddInt64(&c.stats.Hit, 1)
		c.ll.MoveToFront(ele)
		return entry, true
	}

	atomic.AddInt64(&c.stats.Miss, 1)
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
			atomic.AddInt64(&c.stats.UnexpiredEvictions, 1)
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

// UpdateExpiration updates the expiration time for the key
func (c *LRUCache) UpdateExpiration(key string, expireAt int64) bool {
	if ele, hit := c.cache[key]; hit {
		entry := ele.Value.(*CacheItem)
		entry.ExpireAt = expireAt
		return true
	}
	return false
}

func (c *LRUCache) Close() error {
	c.cache = nil
	c.ll = nil
	c.cacheLen = 0
	return nil
}

// Stats returns the current status for the cache
func (c *LRUCache) Stats() CacheStats {
	var result CacheStats
	result.UnexpiredEvictions = atomic.SwapInt64(&c.stats.UnexpiredEvictions, 0)
	result.Miss = atomic.SwapInt64(&c.stats.Miss, 0)
	result.Hit = atomic.SwapInt64(&c.stats.Hit, 0)
	result.Size = atomic.LoadInt64(&c.cacheLen)
	return result
}

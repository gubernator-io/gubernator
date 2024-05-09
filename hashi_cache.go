package gubernator

import (
	lru "github.com/hashicorp/golang-lru/v2/expirable"
)

type HashiCache struct {
	lru    *lru.LRU[string, *CacheItem]
	closed bool
}

func NewHashiCache(size int) *HashiCache {
	c := &HashiCache{
		lru: lru.NewLRU[string, *CacheItem](size, nil, 0),
	}
	return c
}

func (c *HashiCache) Add(item *CacheItem) bool {
	return c.lru.Add(item.Key, item)
}

func (c *HashiCache) UpdateExpiration(key string, expireAt int64) bool {
	item, ok := c.lru.Get(key)
	if !ok {
		return false
	}
	item.ExpireAt = expireAt
	c.lru.Add(key, item)
	return true
}

func (c *HashiCache) GetItem(key string) (*CacheItem, bool) {
	item, ok := c.lru.Get(key)
	if !ok {
		return nil, false
	}

	if item.IsExpired() {
		c.lru.Remove(key)
		return nil, false
	}

	return item, ok
}

func (c *HashiCache) Each() chan *CacheItem {
	ch := make(chan *CacheItem, c.lru.Len())
	for _, item := range c.lru.Keys() {
		val, _ := c.lru.Get(item)
		ch <- val
	}
	close(ch)
	return ch
}

func (c *HashiCache) Remove(key string) {
	c.lru.Remove(key)
}

func (c *HashiCache) Size() int64 {
	return int64(c.lru.Len())
}

func (c *HashiCache) Close() error {
	return nil
}

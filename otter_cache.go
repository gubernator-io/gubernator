package gubernator

import (
	"fmt"

	"github.com/mailgun/holster/v4/setter"
	"github.com/maypok86/otter"
)

type OtterCache struct {
	cache otter.Cache[string, *CacheItem]
}

func NewOtterCache(size int) (*OtterCache, error) {
	setter.SetDefault(&size, 50_000)
	cache, err := otter.MustBuilder[string, *CacheItem](size).Build()
	if err != nil {
		return nil, err
	}
	return &OtterCache{
		cache: cache,
	}, nil
}

func (o *OtterCache) Add(item *CacheItem) bool {
	if o.cache.Set(item.Key, item) {
		return true
	}
	panic(fmt.Sprintf("set dropped on '%s'", item.Key))
}

func (o *OtterCache) GetItem(key string) (*CacheItem, bool) {
	item, ok := o.cache.Get(key)
	if !ok {
		return nil, false
	}

	if item.IsExpired() {
		now := MillisecondNow()
		fmt.Printf("cache item '%s' expired '%d' to '%d'\n", item.Key, item.ExpireAt, now)
		// TODO: Avoid deletion, as that uses a mutex, better to just let the item expire or get set to a new value.
		o.cache.Delete(key)
		return nil, false
	}
	return item, true
}

func (o *OtterCache) UpdateExpiration(key string, expireAt int64) bool {
	item, ok := o.cache.Get(key)
	if !ok {
		return false
	}

	item.ExpireAt = expireAt
	return true
}

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

func (o *OtterCache) Remove(key string) {
	o.cache.Delete(key)
}

func (o *OtterCache) Size() int64 {
	return int64(o.cache.Size())
}

func (o *OtterCache) Close() error {
	o.cache.Close()
	return nil
}

/*
Copyright 2024 Derrick J. Wippler

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
	"context"
	"sync"

	"github.com/pkg/errors"
)

type CacheManager interface {
	GetRateLimit(context.Context, *RateLimitReq, RateLimitReqState) (*RateLimitResp, error)
	GetCacheItem(context.Context, string) (*CacheItem, bool, error)
	AddCacheItem(context.Context, string, *CacheItem) error
	Store(ctx context.Context) error
	Load(context.Context) error
	Close() error
}

type cacheManager struct {
	conf  Config
	cache Cache
}

// NewCacheManager creates a new instance of the CacheManager interface using
// the cache returned by Config.CacheFactory
func NewCacheManager(conf Config) (CacheManager, error) {

	cache, err := conf.CacheFactory(conf.CacheSize)
	if err != nil {
		return nil, err
	}
	return &cacheManager{
		cache: cache,
		conf:  conf,
	}, nil
}

// GetRateLimit fetches the item from the cache if it exists, and preforms the appropriate rate limit calculation
func (m *cacheManager) GetRateLimit(ctx context.Context, req *RateLimitReq, state RateLimitReqState) (*RateLimitResp, error) {
	var rlResponse *RateLimitResp
	var err error

	switch req.Algorithm {
	case Algorithm_TOKEN_BUCKET:
		rlResponse, err = tokenBucket(rateContext{
			Store:    m.conf.Store,
			Cache:    m.cache,
			ReqState: state,
			Request:  req,
			Context:  ctx,
		})
		if err != nil {
			msg := "Error in tokenBucket"
			countError(err, msg)
		}

	case Algorithm_LEAKY_BUCKET:
		rlResponse, err = leakyBucket(rateContext{
			Store:    m.conf.Store,
			Cache:    m.cache,
			ReqState: state,
			Request:  req,
			Context:  ctx,
		})
		if err != nil {
			msg := "Error in leakyBucket"
			countError(err, msg)
		}

	default:
		err = errors.Errorf("Invalid rate limit algorithm '%d'", req.Algorithm)
	}

	return rlResponse, err
}

// Store saves every cache item into persistent storage provided via Config.Loader
func (m *cacheManager) Store(ctx context.Context) error {
	out := make(chan *CacheItem, 500)
	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		for item := range m.cache.Each() {
			select {
			case out <- item:

			case <-ctx.Done():
				return
			}
		}
	}()

	go func() {
		wg.Wait()
		close(out)
	}()

	if ctx.Err() != nil {
		return ctx.Err()
	}

	if err := m.conf.Loader.Save(out); err != nil {
		return errors.Wrap(err, "while calling p.conf.Loader.Save()")
	}
	return nil
}

// Close closes the cache manager
func (m *cacheManager) Close() error {
	return m.cache.Close()
}

// Load cache items from persistent storage provided via Config.Loader
func (m *cacheManager) Load(ctx context.Context) error {
	ch, err := m.conf.Loader.Load()
	if err != nil {
		return errors.Wrap(err, "Error in loader.Load")
	}

	for {
		var item *CacheItem
		var ok bool

		select {
		case item, ok = <-ch:
			if !ok {
				return nil
			}
		case <-ctx.Done():
			return ctx.Err()
		}
		_ = m.cache.Add(item)
	}
}

// GetCacheItem returns an item from the cache
func (m *cacheManager) GetCacheItem(_ context.Context, key string) (*CacheItem, bool, error) {
	item, ok := m.cache.GetItem(key)
	return item, ok, nil
}

// AddCacheItem adds an item to the cache. The CacheItem.Key should be set correctly, else the item
// will not be added to the cache correctly.
func (m *cacheManager) AddCacheItem(_ context.Context, _ string, item *CacheItem) error {
	_ = m.cache.Add(item)
	return nil
}

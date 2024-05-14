/*
Copyright 2018-2022 Mailgun Technologies Inc

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
)

// PERSISTENT STORE DETAILS

// The storage interfaces defined here allows the implementor flexibility in storage options. Depending on the
// use case an implementor can only implement the `Loader` interface and only support persistence of
// ratelimits at startup and shutdown or implement `Store` and  gubernator will continuously call `OnChange()`
// and `Get()` to keep the in memory cache and persistent store up to date with the latest ratelimit data.
// Both interfaces can be implemented simultaneously to ensure data is always saved to persistent storage.

// LeakyBucketItem is 40 bytes aligned in size
type LeakyBucketItem struct {
	Limit     int64   // 8 bytes
	Duration  int64   // 8 bytes
	Remaining float64 // 8 bytes
	UpdatedAt int64   // 8 bytes
	Burst     int64   // 8 bytes
}

// TokenBucketItem is 40 bytes aligned in size
type TokenBucketItem struct {
	Limit     int64  // 8 bytes
	Duration  int64  // 8 bytes
	Remaining int64  // 8 bytes
	CreatedAt int64  // 8 bytes
	Status    Status // 4 bytes
	// 4 bytes of padding
}

// Store interface allows implementors to off load storage of all or a subset of ratelimits to
// some persistent store. Methods OnChange() and Remove() should avoid blocking where possible
// to maximize performance of gubernator.
// Implementations MUST be threadsafe.
type Store interface {
	// OnChange is called by gubernator *after* a rate limit item is updated. It's up to the store to
	// decide if this rate limit item should be persisted in the store. It's up to the
	// store to expire old rate limit items. The CacheItem represents the current state of
	// the rate limit item *after* the RateLimitReq has been applied.
	OnChange(ctx context.Context, r *RateLimitReq, item *CacheItem)

	// Get is called by gubernator when a rate limit is missing from the cache. It's up to the store
	// to decide if this request is fulfilled. Should return true if the request is fulfilled
	// and false if the request is not fulfilled or doesn't exist in the store.
	Get(ctx context.Context, r *RateLimitReq) (*CacheItem, bool)

	// Remove ic called by gubernator when an existing rate limit should be removed from the store.
	// NOTE: This is NOT called when an rate limit expires from the cache, store implementors
	// must expire rate limits in the store.
	Remove(ctx context.Context, key string)
}

// Loader interface allows implementors to store all or a subset of ratelimits into a persistent
// store during startup and shutdown of the gubernator instance.
type Loader interface {
	// Load is called by gubernator just before the instance is ready to accept requests. The implementation
	// should return a channel gubernator can read to load all rate limits that should be loaded into the
	// instance cache. The implementation should close the channel to indicate no more rate limits left to load.
	Load() (chan *CacheItem, error)

	// Save is called by gubernator just before the instance is shutdown. The passed channel should be
	// read until the channel is closed.
	Save(chan *CacheItem) error
}

func NewMockLoader() *MockLoader {
	ml := &MockLoader{
		Called: make(map[string]int),
	}
	ml.Called["Load()"] = 0
	ml.Called["Save()"] = 0
	return ml
}

type MockLoader struct {
	Called     map[string]int
	CacheItems []*CacheItem
}

var _ Loader = &MockLoader{}

func (ml *MockLoader) Load() (chan *CacheItem, error) {
	ml.Called["Load()"] += 1

	ch := make(chan *CacheItem, 10)
	go func() {
		for _, i := range ml.CacheItems {
			ch <- i
		}
		close(ch)
	}()
	return ch, nil
}

func (ml *MockLoader) Save(in chan *CacheItem) error {
	ml.Called["Save()"] += 1

	for i := range in {
		ml.CacheItems = append(ml.CacheItems, i)
	}
	return nil
}

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

import "sync"

type Cache interface {
	Add(item *CacheItem) bool
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

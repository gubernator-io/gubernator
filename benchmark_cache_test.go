package gubernator_test

import (
	"math/rand"
	"sync"
	"testing"
	"time"

	"github.com/gubernator-io/gubernator/v2"
	"github.com/mailgun/holster/v4/clock"
	"github.com/stretchr/testify/require"
)

func BenchmarkCache(b *testing.B) {
	const defaultNumKeys = 8192
	testCases := []struct {
		Name         string
		NewTestCache func() (gubernator.Cache, error)
		LockRequired bool
	}{
		{
			Name: "LRUCache",
			NewTestCache: func() (gubernator.Cache, error) {
				return gubernator.NewLRUCache(0), nil
			},
			LockRequired: true,
		},
		{
			Name: "OtterCache",
			NewTestCache: func() (gubernator.Cache, error) {
				return gubernator.NewOtterCache(0)
			},
			LockRequired: false,
		},
	}

	for _, testCase := range testCases {
		b.Run(testCase.Name, func(b *testing.B) {
			b.Run("Sequential reads", func(b *testing.B) {
				cache, err := testCase.NewTestCache()
				require.NoError(b, err)
				expire := clock.Now().Add(time.Hour).UnixMilli()
				keys := GenerateRandomKeys(defaultNumKeys)

				for _, key := range keys {
					item := &gubernator.CacheItem{
						Key:      key,
						Value:    "value:" + key,
						ExpireAt: expire,
					}
					cache.Add(item)
				}

				mask := len(keys) - 1
				b.ReportAllocs()
				b.ResetTimer()

				for i := 0; i < b.N; i++ {
					index := int(rand.Uint32() & uint32(mask))
					_, _ = cache.GetItem(keys[index&mask])
				}
			})

			b.Run("Sequential writes", func(b *testing.B) {
				cache, err := testCase.NewTestCache()
				require.NoError(b, err)
				expire := clock.Now().Add(time.Hour).UnixMilli()
				keys := GenerateRandomKeys(defaultNumKeys)

				mask := len(keys) - 1
				b.ReportAllocs()
				b.ResetTimer()

				for i := 0; i < b.N; i++ {
					index := int(rand.Uint32() & uint32(mask))
					item := &gubernator.CacheItem{
						Key:      keys[index&mask],
						Value:    "value:" + keys[index&mask],
						ExpireAt: expire,
					}
					cache.Add(item)
				}
			})

			b.Run("Concurrent reads", func(b *testing.B) {
				cache, err := testCase.NewTestCache()
				require.NoError(b, err)
				expire := clock.Now().Add(time.Hour).UnixMilli()
				keys := GenerateRandomKeys(defaultNumKeys)

				for _, key := range keys {
					item := &gubernator.CacheItem{
						Key:      key,
						Value:    "value:" + key,
						ExpireAt: expire,
					}
					cache.Add(item)
				}

				var mutex sync.Mutex
				var task func(key string)

				if testCase.LockRequired {
					task = func(key string) {
						mutex.Lock()
						defer mutex.Unlock()
						_, _ = cache.GetItem(key)
					}
				} else {
					task = func(key string) {
						_, _ = cache.GetItem(key)
					}
				}

				b.ReportAllocs()
				b.ResetTimer()

				mask := len(keys) - 1

				b.RunParallel(func(pb *testing.PB) {
					index := int(rand.Uint32() & uint32(mask))
					for pb.Next() {
						task(keys[index&mask])
					}
				})

			})

			b.Run("Concurrent writes", func(b *testing.B) {
				cache, err := testCase.NewTestCache()
				require.NoError(b, err)
				expire := clock.Now().Add(time.Hour).UnixMilli()
				keys := GenerateRandomKeys(defaultNumKeys)

				var mutex sync.Mutex
				var task func(key string)

				if testCase.LockRequired {
					task = func(key string) {
						mutex.Lock()
						defer mutex.Unlock()
						item := &gubernator.CacheItem{
							Key:      key,
							Value:    "value:" + key,
							ExpireAt: expire,
						}
						cache.Add(item)
					}
				} else {
					task = func(key string) {
						item := &gubernator.CacheItem{
							Key:      key,
							Value:    "value:" + key,
							ExpireAt: expire,
						}
						cache.Add(item)
					}
				}

				mask := len(keys) - 1
				b.ReportAllocs()
				b.ResetTimer()

				b.RunParallel(func(pb *testing.PB) {
					index := int(rand.Uint32() & uint32(mask))
					for pb.Next() {
						task(keys[index&mask])
					}
				})
			})

		})
	}
}

func GenerateRandomKeys(size int) []string {
	keys := make([]string, 0, size)
	for i := 0; i < size; i++ {
		keys = append(keys, gubernator.RandomString(20))
	}
	return keys
}

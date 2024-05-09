package gubernator_test

import (
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/gubernator-io/gubernator/v2"
	"github.com/mailgun/holster/v4/clock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHashiLRUCache(t *testing.T) {
	const iterations = 1000
	const concurrency = 100
	expireAt := clock.Now().Add(1 * time.Hour).UnixMilli()

	t.Run("Happy path", func(t *testing.T) {
		cache := gubernator.NewHashiCache(0)

		// Populate cache.
		for i := 0; i < iterations; i++ {
			key := strconv.Itoa(i)
			item := &gubernator.CacheItem{
				Key:      key,
				Value:    i,
				ExpireAt: expireAt,
			}
			exists := cache.Add(item)
			assert.False(t, exists)
		}

		// Validate cache.
		assert.Equal(t, int64(iterations), cache.Size())

		for i := 0; i < iterations; i++ {
			key := strconv.Itoa(i)
			item, ok := cache.GetItem(key)
			require.True(t, ok)
			require.NotNil(t, item)
			assert.Equal(t, item.Value, i)
		}

		// Clear cache.
		for i := 0; i < iterations; i++ {
			key := strconv.Itoa(i)
			cache.Remove(key)
		}

		assert.Zero(t, cache.Size())
	})

	t.Run("Update an existing key", func(t *testing.T) {
		cache := gubernator.NewHashiCache(0)
		const key = "foobar"

		// Add key.
		item1 := &gubernator.CacheItem{
			Key:      key,
			Value:    "initial value",
			ExpireAt: expireAt,
		}
		exists1 := cache.Add(item1)
		require.False(t, exists1)

		// Update same key.
		item2 := &gubernator.CacheItem{
			Key:      key,
			Value:    "new value",
			ExpireAt: expireAt,
		}
		evicted := cache.Add(item2)
		require.False(t, evicted)

		// Verify.
		verifyItem, ok := cache.GetItem(key)
		require.True(t, ok)
		assert.Equal(t, item2, verifyItem)
	})

	t.Run("Concurrent reads", func(t *testing.T) {
		cache := gubernator.NewHashiCache(0)

		// Populate cache.
		for i := 0; i < iterations; i++ {
			key := strconv.Itoa(i)
			item := &gubernator.CacheItem{
				Key:      key,
				Value:    i,
				ExpireAt: expireAt,
			}
			exists := cache.Add(item)
			assert.False(t, exists)
		}

		assert.Equal(t, int64(iterations), cache.Size())
		var launchWg, doneWg sync.WaitGroup
		launchWg.Add(1)

		for thread := 0; thread < concurrency; thread++ {
			doneWg.Add(1)

			go func() {
				defer doneWg.Done()
				launchWg.Wait()

				for i := 0; i < iterations; i++ {
					key := strconv.Itoa(i)
					item, ok := cache.GetItem(key)
					assert.True(t, ok)
					require.NotNil(t, item)
					assert.Equal(t, item.Value, i)
				}
			}()
		}

		// Wait for goroutines to finish.
		launchWg.Done()
		doneWg.Wait()
	})

	t.Run("Concurrent writes", func(t *testing.T) {
		cache := gubernator.NewHashiCache(0)
		expireAt := clock.Now().Add(1 * time.Hour).UnixMilli()
		var launchWg, doneWg sync.WaitGroup
		launchWg.Add(1)

		for thread := 0; thread < concurrency; thread++ {
			doneWg.Add(1)

			go func() {
				defer doneWg.Done()
				launchWg.Wait()

				for i := 0; i < iterations; i++ {
					key := strconv.Itoa(i)
					item := &gubernator.CacheItem{
						Key:      key,
						Value:    i,
						ExpireAt: expireAt,
					}
					cache.Add(item)
				}
			}()
		}

		// Wait for goroutines to finish.
		launchWg.Done()
		doneWg.Wait()
	})

	t.Run("Concurrent reads and writes", func(t *testing.T) {
		cache := gubernator.NewHashiCache(0)

		// Populate cache.
		for i := 0; i < iterations; i++ {
			key := strconv.Itoa(i)
			item := &gubernator.CacheItem{
				Key:      key,
				Value:    i,
				ExpireAt: expireAt,
			}
			exists := cache.Add(item)
			assert.False(t, exists)
		}

		assert.Equal(t, int64(iterations), cache.Size())
		var launchWg, doneWg sync.WaitGroup
		launchWg.Add(1)

		for thread := 0; thread < concurrency; thread++ {
			doneWg.Add(2)

			go func() {
				defer doneWg.Done()
				launchWg.Wait()

				for i := 0; i < iterations; i++ {
					key := strconv.Itoa(i)
					item, ok := cache.GetItem(key)
					assert.True(t, ok)
					require.NotNil(t, item)
					assert.Equal(t, item.Value, i)
				}
			}()

			go func() {
				defer doneWg.Done()
				launchWg.Wait()

				for i := 0; i < iterations; i++ {
					key := strconv.Itoa(i)
					item := &gubernator.CacheItem{
						Key:      key,
						Value:    i,
						ExpireAt: expireAt,
					}
					cache.Add(item)
				}
			}()
		}

		// Wait for goroutines to finish.
		launchWg.Done()
		doneWg.Wait()
	})
}

func BenchmarkHashiLRUCache(b *testing.B) {

	b.Run("Sequential reads", func(b *testing.B) {
		cache := gubernator.NewHashiCache(b.N)
		expireAt := clock.Now().Add(1 * time.Hour).UnixMilli()

		// Populate cache.
		for i := 0; i < b.N; i++ {
			key := strconv.Itoa(i)
			item := &gubernator.CacheItem{
				Key:      key,
				Value:    i,
				ExpireAt: expireAt,
			}
			exists := cache.Add(item)
			assert.False(b, exists)
		}

		b.ReportAllocs()
		b.ResetTimer()

		for i := 0; i < b.N; i++ {
			key := strconv.Itoa(i)
			_, _ = cache.GetItem(key)
		}
	})

	b.Run("Sequential writes", func(b *testing.B) {
		cache := gubernator.NewHashiCache(0)
		expireAt := clock.Now().Add(1 * time.Hour).UnixMilli()

		b.ReportAllocs()
		b.ResetTimer()

		for i := 0; i < b.N; i++ {
			key := strconv.Itoa(i)
			item := &gubernator.CacheItem{
				Key:      key,
				Value:    i,
				ExpireAt: expireAt,
			}
			cache.Add(item)
		}
	})

	b.Run("Concurrent reads", func(b *testing.B) {
		cache := gubernator.NewHashiCache(b.N)
		expireAt := clock.Now().Add(1 * time.Hour).UnixMilli()

		// Populate cache.
		for i := 0; i < b.N; i++ {
			key := strconv.Itoa(i)
			item := &gubernator.CacheItem{
				Key:      key,
				Value:    i,
				ExpireAt: expireAt,
			}
			exists := cache.Add(item)
			assert.False(b, exists)
		}

		var launchWg, doneWg sync.WaitGroup
		launchWg.Add(1)

		for i := 0; i < b.N; i++ {
			key := strconv.Itoa(i)
			doneWg.Add(1)

			go func() {
				defer doneWg.Done()
				launchWg.Wait()

				_, _ = cache.GetItem(key)
			}()
		}

		b.ReportAllocs()
		b.ResetTimer()
		launchWg.Done()
		doneWg.Wait()
	})

	b.Run("Concurrent writes", func(b *testing.B) {
		cache := gubernator.NewHashiCache(0)
		expireAt := clock.Now().Add(1 * time.Hour).UnixMilli()
		var launchWg, doneWg sync.WaitGroup
		launchWg.Add(1)

		for i := 0; i < b.N; i++ {
			key := strconv.Itoa(i)
			doneWg.Add(1)

			go func(i int) {
				defer doneWg.Done()
				launchWg.Wait()

				item := &gubernator.CacheItem{
					Key:      key,
					Value:    i,
					ExpireAt: expireAt,
				}
				cache.Add(item)
			}(i)
		}

		b.ReportAllocs()
		b.ResetTimer()
		launchWg.Done()
		doneWg.Wait()
	})

	b.Run("Concurrent reads and writes of existing keys", func(b *testing.B) {
		cache := gubernator.NewHashiCache(0)
		expireAt := clock.Now().Add(1 * time.Hour).UnixMilli()
		var launchWg, doneWg sync.WaitGroup
		launchWg.Add(1)

		// Populate cache.
		for i := 0; i < b.N; i++ {
			key := strconv.Itoa(i)
			item := &gubernator.CacheItem{
				Key:      key,
				Value:    i,
				ExpireAt: expireAt,
			}
			exists := cache.Add(item)
			assert.False(b, exists)
		}

		for i := 0; i < b.N; i++ {
			key := strconv.Itoa(i)
			doneWg.Add(2)

			go func() {
				defer doneWg.Done()
				launchWg.Wait()

				_, _ = cache.GetItem(key)
			}()

			go func(i int) {
				defer doneWg.Done()
				launchWg.Wait()

				item := &gubernator.CacheItem{
					Key:      key,
					Value:    i,
					ExpireAt: expireAt,
				}
				cache.Add(item)
			}(i)
		}

		b.ReportAllocs()
		b.ResetTimer()
		launchWg.Done()
		doneWg.Wait()
	})

	b.Run("Concurrent reads and writes of non-existent keys", func(b *testing.B) {
		cache := gubernator.NewHashiCache(0)
		expireAt := clock.Now().Add(1 * time.Hour).UnixMilli()
		var launchWg, doneWg sync.WaitGroup
		launchWg.Add(1)

		for i := 0; i < b.N; i++ {
			doneWg.Add(2)

			go func(i int) {
				defer doneWg.Done()
				launchWg.Wait()

				key := strconv.Itoa(i)
				_, _ = cache.GetItem(key)
			}(i)

			go func(i int) {
				defer doneWg.Done()
				launchWg.Wait()

				key := "z" + strconv.Itoa(i)
				item := &gubernator.CacheItem{
					Key:      key,
					Value:    i,
					ExpireAt: expireAt,
				}
				cache.Add(item)
			}(i)
		}

		b.ReportAllocs()
		b.ResetTimer()
		launchWg.Done()
		doneWg.Wait()
	})
}

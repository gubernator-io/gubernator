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

func TestOtterCache(t *testing.T) {
	const iterations = 1000
	const concurrency = 100
	expireAt := clock.Now().Add(1 * time.Hour).UnixMilli()

	t.Run("Happy path", func(t *testing.T) {
		cache, err := gubernator.NewOtterCache(0)
		require.NoError(t, err)

		// Populate cache.
		for i := 0; i < iterations; i++ {
			key := strconv.Itoa(i)
			item := &gubernator.CacheItem{
				Key:      key,
				Value:    i,
				ExpireAt: expireAt,
			}
			cache.AddIfNotPresent(item)
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
		cache, err := gubernator.NewOtterCache(0)
		require.NoError(t, err)
		const key = "foobar"

		// Add key.
		item1 := &gubernator.CacheItem{
			Key:      key,
			Value:    "initial value",
			ExpireAt: expireAt,
		}
		cache.AddIfNotPresent(item1)

		// Update same key is refused
		item2 := &gubernator.CacheItem{
			Key:      key,
			Value:    "new value",
			ExpireAt: expireAt,
		}
		assert.False(t, cache.AddIfNotPresent(item2))

		// Fetch and update the CacheItem
		update, ok := cache.GetItem(key)
		assert.True(t, ok)
		update.Value = "new value"

		// Verify.
		verifyItem, ok := cache.GetItem(key)
		require.True(t, ok)
		assert.Equal(t, item2, verifyItem)
	})

	t.Run("Concurrent reads", func(t *testing.T) {
		cache, err := gubernator.NewOtterCache(0)
		require.NoError(t, err)

		// Populate cache.
		for i := 0; i < iterations; i++ {
			key := strconv.Itoa(i)
			item := &gubernator.CacheItem{
				Key:      key,
				Value:    i,
				ExpireAt: expireAt,
			}
			cache.AddIfNotPresent(item)
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
		cache, err := gubernator.NewOtterCache(0)
		require.NoError(t, err)
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
					cache.AddIfNotPresent(item)
				}
			}()
		}

		// Wait for goroutines to finish.
		launchWg.Done()
		doneWg.Wait()
	})

	t.Run("Concurrent reads and writes", func(t *testing.T) {
		cache, err := gubernator.NewOtterCache(0)
		require.NoError(t, err)

		// Populate cache.
		for i := 0; i < iterations; i++ {
			key := strconv.Itoa(i)
			item := &gubernator.CacheItem{
				Key:      key,
				Value:    i,
				ExpireAt: expireAt,
			}
			cache.AddIfNotPresent(item)
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

				// Write different keys than the keys we are reading to avoid race on Add() / GetItem()
				for i := iterations; i < iterations*2; i++ {
					key := strconv.Itoa(i)
					item := &gubernator.CacheItem{
						Key:      key,
						Value:    i,
						ExpireAt: expireAt,
					}
					cache.AddIfNotPresent(item)
				}
			}()
		}

		// Wait for goroutines to finish.
		launchWg.Done()
		doneWg.Wait()
	})
}

func BenchmarkOtterCache(b *testing.B) {

	b.Run("Sequential reads", func(b *testing.B) {
		cache, _ := gubernator.NewOtterCache(0)

		expireAt := clock.Now().Add(1 * time.Hour).UnixMilli()

		// Populate cache.
		for i := 0; i < b.N; i++ {
			key := strconv.Itoa(i)
			item := &gubernator.CacheItem{
				Key:      key,
				Value:    i,
				ExpireAt: expireAt,
			}
			cache.AddIfNotPresent(item)
		}

		b.ReportAllocs()
		b.ResetTimer()

		for i := 0; i < b.N; i++ {
			key := strconv.Itoa(i)
			_, _ = cache.GetItem(key)
		}
	})

	b.Run("Sequential writes", func(b *testing.B) {
		cache, _ := gubernator.NewOtterCache(0)
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
			cache.AddIfNotPresent(item)
		}
	})

	b.Run("Concurrent reads", func(b *testing.B) {
		cache, _ := gubernator.NewOtterCache(0)
		expireAt := clock.Now().Add(1 * time.Hour).UnixMilli()

		// Populate cache.
		for i := 0; i < b.N; i++ {
			key := strconv.Itoa(i)
			item := &gubernator.CacheItem{
				Key:      key,
				Value:    i,
				ExpireAt: expireAt,
			}
			cache.AddIfNotPresent(item)
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
		cache, _ := gubernator.NewOtterCache(0)
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
				cache.AddIfNotPresent(item)
			}(i)
		}

		b.ReportAllocs()
		b.ResetTimer()
		launchWg.Done()
		doneWg.Wait()
	})

	b.Run("Concurrent reads and writes of existing keys", func(b *testing.B) {
		cache, _ := gubernator.NewOtterCache(0)
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
			cache.AddIfNotPresent(item)
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
				cache.AddIfNotPresent(item)
			}(i)
		}

		b.ReportAllocs()
		b.ResetTimer()
		launchWg.Done()
		doneWg.Wait()
	})

	b.Run("Concurrent reads and writes of non-existent keys", func(b *testing.B) {
		cache, _ := gubernator.NewOtterCache(0)
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
				cache.AddIfNotPresent(item)
			}(i)
		}

		b.ReportAllocs()
		b.ResetTimer()
		launchWg.Done()
		doneWg.Wait()
	})
}

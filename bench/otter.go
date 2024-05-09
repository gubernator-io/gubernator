package bench

import (
	"context"
	"math/rand"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/gubernator-io/gubernator/v2"
)

func OtterPreLoad(name string, keys []string) (*gubernator.WorkerOtter, error) {
	l := &MockLoader{}
	p := gubernator.NewWorkerOtter(gubernator.Config{
		CacheSize: cacheSize * 2,
		Loader:    l,
	})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second*10)
	defer cancel()

	// Prefill the cache
	createdAt := time.Now().UnixNano() / 1_000_000
	for _, k := range keys {
		_, err := p.GetRateLimit(ctx, &gubernator.RateLimitReq{
			CreatedAt: &createdAt,
			Duration:  100_000,
			Name:      name,
			UniqueKey: k,
		}, gubernator.RateLimitReqState{})
		if err != nil {
			return nil, err
		}
	}

	//if err := p.Store(ctx); err != nil {
	//	return nil, err
	//}
	//
	//if l.Count != cacheSize {
	//	return nil, fmt.Errorf("item count '%d' in pool does not match expected size of '%d'", l.Count, cacheSize)
	//}

	return p, nil
}

func OtterReadParallel(b *testing.B, processors int,
	otter *gubernator.WorkerOtter, createdAt *int64, keys []string) {

	runtime.GOMAXPROCS(processors)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second*10)
	defer cancel()

	mask := len(keys) - 1
	start := time.Now()
	b.ResetTimer()

	b.RunParallel(func(pb *testing.PB) {
		index := int(rand.Uint32() & uint32(mask))

		for pb.Next() {
			_, err := otter.GetRateLimit(ctx, &gubernator.RateLimitReq{
				UniqueKey: keys[index&mask],
				CreatedAt: createdAt,
				Name:      b.Name(),
				Duration:  100_000,
			}, gubernator.RateLimitReqState{})
			index++
			if err != nil {
				b.Error(err)
				return
			}
		}
	})
	opsPerSec := float64(b.N) / time.Since(start).Seconds()
	b.ReportMetric(opsPerSec, "ops/s")
}

func OtterWriteParallel(b *testing.B, processors int) {
	runtime.GOMAXPROCS(processors)

	p := gubernator.NewWorkerOtter(gubernator.Config{
		CacheSize: cacheSize * 1_000_000,
	})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second*10)
	defer cancel()

	createdAt := time.Now().UnixNano() / 1_000_000
	keys := GenerateRandomKeys()
	mask := len(keys) - 1
	start := time.Now()
	b.ResetTimer()

	b.RunParallel(func(pb *testing.PB) {
		index := int(rand.Uint32() & uint32(mask))

		for pb.Next() {
			_, err := p.GetRateLimit(ctx, &gubernator.RateLimitReq{
				CreatedAt: &createdAt,
				UniqueKey: keys[index&mask],
				Duration:  100_000,
				Name:      b.Name(),
			}, gubernator.RateLimitReqState{})
			index++
			if err != nil {
				b.Error(err)
				return
			}
		}

	})
	opsPerSec := float64(b.N) / time.Since(start).Seconds()
	b.ReportMetric(opsPerSec, "ops/s")
}

func OtterRead(b *testing.B, concurrency int) {
	// Ensure the size of the data in the pool is consistent throughout all concurrency levels
	const size = 1_000_000
	l := &MockLoader{}
	p := gubernator.NewWorkerOtter(gubernator.Config{
		CacheSize: size * 1_000_000,
		Loader:    l,
	})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second*10)
	defer cancel()

	var keys []string

	createdAt := time.Now().UnixNano() / 1_000_000
	for i := 0; i < size; i++ {
		key := gubernator.RandomString(10)
		keys = append(keys, key)
		_, err := p.GetRateLimit(ctx, &gubernator.RateLimitReq{
			CreatedAt: &createdAt,
			Duration:  100_000,
			UniqueKey: key,
			Name:      key,
		}, gubernator.RateLimitReqState{})
		if err != nil {
			b.Fatal(err)
		}
	}

	if err := p.Store(ctx); err != nil {
		b.Fatal(err)
	}

	if l.Count != size {
		b.Fatal("item count in pool does not match expected size")
	}

	b.ResetTimer()
	var wg, launched sync.WaitGroup
	launched.Add(concurrency)
	for thread := 0; thread < concurrency; thread++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Wait here until all go routines are created
			launched.Wait()
			for i := 0; i < b.N; i++ {
				_, err := p.GetRateLimit(ctx, &gubernator.RateLimitReq{
					CreatedAt: &createdAt,
					Duration:  100_000,
					UniqueKey: keys[i],
					Name:      b.Name(),
				}, gubernator.RateLimitReqState{})
				if err != nil {
					b.Error(err)
					return
				}
			}
		}()
		launched.Done()
	}
	wg.Wait()
}

func OtterWrite(b *testing.B, concurrency int) {
	// Ensure the size of the data in the pool is consistent throughout all concurrency levels
	const size = 1_000_000
	l := &MockLoader{}
	p := gubernator.NewWorkerOtter(gubernator.Config{
		CacheSize: size * 1_000_000,
		Loader:    l,
	})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second*10)
	defer cancel()

	var keys []string

	createdAt := time.Now().UnixNano() / 1_000_000
	for i := 0; i < size; i++ {
		key := gubernator.RandomString(10)
		keys = append(keys, key)
	}

	b.ResetTimer()
	var wg, launched sync.WaitGroup
	launched.Add(concurrency)
	for thread := 0; thread < concurrency; thread++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Wait here until all go routines are created
			launched.Wait()
			for i := 0; i < b.N; i++ {
				_, err := p.GetRateLimit(ctx, &gubernator.RateLimitReq{
					CreatedAt: &createdAt,
					Duration:  100_000,
					UniqueKey: keys[i],
					Name:      b.Name(),
				}, gubernator.RateLimitReqState{})
				if err != nil {
					b.Error(err)
					return
				}
			}
		}()
		launched.Done()
	}
	wg.Wait()
}

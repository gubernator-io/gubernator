package bench_test

import (
	"context"
	"fmt"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/gubernator-io/gubernator/v2"
	"github.com/gubernator-io/gubernator/v2/bench"
	"github.com/sirupsen/logrus"
)

//func BenchmarkAccessStructure(b *testing.B) {
//	for _, size := range []int{1, 10, 100, 1000, 10000, 100000, 1000000} {
//		benchmarkAccessStructure(b, size)
//	}
//}
//
//func benchmarkAccessStructure(b *testing.B, size int) {
//	var indexes = make([]int, size, size)
//	var arr = make([]int, size, size)
//	var hash = make(map[int]int)
//
//	//rand.Seed(size % 42)
//	for i := 0; i < size; i++ {
//		indexes[i] = rand.Intn(size)
//		arr[i] = i
//		hash[i] = i
//	}
//
//	b.ResetTimer()
//
//	b.Run(fmt.Sprintf("Array_%d", size), func(b *testing.B) {
//		for i := 0; i < b.N; i++ {
//			indx := indexes[i%size] % size
//			_ = arr[indx]
//		}
//	})
//
//	b.Run(fmt.Sprintf("Hash_%d", size), func(b *testing.B) {
//		for i := 0; i < b.N; i++ {
//			indx := indexes[i%size] % size
//			_ = hash[indx]
//		}
//	})
//}

func BenchmarkWorkerPool(b *testing.B) {
	for _, con := range []int{1, 10, 100, 1000, 10_000, 100_000} {
		b.Run(fmt.Sprintf("Read_%d", con), func(b *testing.B) {
			benchmarkWorkerPoolRead(b, con)
		})
	}
	for _, con := range []int{1, 10, 100, 1000, 10_000, 100_000} {
		b.Run(fmt.Sprintf("Write_%d", con), func(b *testing.B) {
			benchmarkWorkerPoolWrite(b, con)
		})
	}
}

func benchmarkWorkerPoolRead(b *testing.B, concurrency int) {
	// Ensure the size of the data in the pool is consistent throughout all concurrency levels
	const size = 1_000_000
	l := &bench.MockLoader{}
	p := gubernator.NewWorkerPool(&gubernator.Config{
		CacheFactory: func(maxSize int) gubernator.Cache {
			return gubernator.NewLRUCache(maxSize)
		},
		CacheSize: size * 1_000_000,
		Workers:   runtime.NumCPU(),
		Logger:    logrus.New(),
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

func benchmarkWorkerPoolWrite(b *testing.B, concurrency int) {
	// Ensure the size of the data in the pool is consistent throughout all concurrency levels
	const size = 1_000_000
	l := &bench.MockLoader{}
	p := gubernator.NewWorkerPool(&gubernator.Config{
		CacheFactory: func(maxSize int) gubernator.Cache {
			return gubernator.NewLRUCache(maxSize)
		},
		CacheSize: size * 1_000_000,
		Workers:   runtime.NumCPU(),
		Logger:    logrus.New(),
		Loader:    l,
	})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second*10)
	defer cancel()

	var keys []string

	createdAt := time.Now().UnixNano() / 1_000_000
	for i := 0; i < size; i++ {
		key := gubernator.RandomString(10)
		keys = append(keys, key)
		//_, err := p.GetRateLimit(ctx, &gubernator.RateLimitReq{
		//	CreatedAt: &createdAt,
		//	UniqueKey: key,
		//	Name:      key,
		//}, gubernator.RateLimitReqState{})
		//if err != nil {
		//	b.Fatal(err)
		//}
	}

	//if err := p.Store(ctx); err != nil {
	//	b.Fatal(err)
	//}
	//
	//if l.Count != size {
	//	b.Fatal("item count in pool does not match expected size")
	//}

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

// This is baseline comparison to a similar test in /benchmark_test.go called 'BenchmarkServer/Thundering_herd-10'
//func BenchmarkThunderingHerd(b *testing.B) {
//	// Ensure the size of the data in the pool is consistent throughout all concurrency levels
//	const size = 1_000_000
//	p := gubernator.NewWorkerPool(&gubernator.Config{
//		CacheFactory: func(maxSize int) gubernator.Cache {
//			return gubernator.NewLRUCache(maxSize)
//		},
//		CacheSize: size * 1_000_000,
//		Workers:   runtime.NumCPU(),
//		Logger:    logrus.New(),
//	})
//	ctx, cancel := context.WithTimeout(context.Background(), time.Second*10)
//	defer cancel()
//
//	createdAt := time.Now().UnixNano() / 1_000_000
//
//	b.ResetTimer()
//	fan := syncutil.NewFanOut(100)
//
//	for n := 0; n < b.N; n++ {
//		fan.Run(func(o interface{}) error {
//			_, err := p.GetRateLimit(ctx, &gubernator.RateLimitReq{
//				CreatedAt: &createdAt,
//				UniqueKey: gubernator.RandomString(10),
//				Name:      b.Name(),
//			}, gubernator.RateLimitReqState{})
//			if err != nil {
//				b.Errorf("Error in client.GetRateLimits: %s", err)
//			}
//			return nil
//		}, nil)
//	}
//
//	fan.Wait()
//}

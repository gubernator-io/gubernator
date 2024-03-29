package bench_test

import (
	"fmt"
	"testing"

	"github.com/gubernator-io/gubernator/v2/bench"
)

func BenchmarkAccessStructure(b *testing.B) {
	for _, size := range []int{1, 10, 100, 1000, 10000, 100000, 1000000} {
		bench.AccessStructure(b, size)
	}
}

func BenchmarkConcurrency(b *testing.B) {
	for _, con := range []int{1, 10, 100, 1000, 5_000, 10_000, 15_000, 20_000} {
		b.Run(fmt.Sprintf("NoCache_%d", con), func(b *testing.B) {
			bench.NoCache(b, con)
		})
	}
	for _, con := range []int{1, 10, 100, 1000, 5_000, 10_000, 15_000, 20_000} {
		b.Run(fmt.Sprintf("OtterWrite_%d", con), func(b *testing.B) {
			bench.OtterWrite(b, con)
		})
	}
	for _, con := range []int{1, 10, 100, 1000, 5_000, 10_000, 15_000, 20_000} {
		b.Run(fmt.Sprintf("OtterRead_%d", con), func(b *testing.B) {
			bench.OtterRead(b, con)
		})
	}
	for _, con := range []int{1, 10, 100, 1000, 5_000, 10_000, 15_000, 20_000} {
		b.Run(fmt.Sprintf("MutexRead_%d", con), func(b *testing.B) {
			bench.MutexRead(b, con)
		})
	}
	for _, con := range []int{1, 10, 100, 1000, 5_000, 10_000, 15_000, 20_000} {
		b.Run(fmt.Sprintf("MutexWrite_%d", con), func(b *testing.B) {
			bench.MutexWrite(b, con)
		})
	}
	for _, con := range []int{1, 10, 100, 1000, 5_000, 10_000, 15_000, 20_000} {
		b.Run(fmt.Sprintf("WorkerPoolRead_%d", con), func(b *testing.B) {
			bench.WorkerPoolRead(b, con)
		})
	}
	for _, con := range []int{1, 10, 100, 1000, 5_000, 10_000, 15_000, 20_000} {
		b.Run(fmt.Sprintf("WorkerPoolWrite_%d", con), func(b *testing.B) {
			bench.WorkerPoolWrite(b, con)
		})
	}
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

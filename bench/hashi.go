package bench

import (
	"context"
	"math/rand"
	"runtime"
	"testing"
	"time"

	"github.com/gubernator-io/gubernator/v2"
)

func HashiReadParallel(b *testing.B, processors int) {
	runtime.GOMAXPROCS(processors)

	l := &MockLoader{}
	p := gubernator.NewWorkerHashi(gubernator.Config{
		CacheFactory: func(maxSize int) gubernator.Cache {
			return gubernator.NewHashiCache(maxSize)
		},
		CacheSize: cacheSize * 1_000_000,
		Loader:    l,
	})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second*10)
	defer cancel()

	// Prefill the cache
	createdAt := time.Now().UnixNano() / 1_000_000
	keys := GenerateRandomKeys()
	for _, k := range keys {
		_, err := p.GetRateLimit(ctx, &gubernator.RateLimitReq{
			CreatedAt: &createdAt,
			Name:      b.Name(),
			Duration:  10_000,
			UniqueKey: k,
		}, gubernator.RateLimitReqState{})
		if err != nil {
			b.Fatal(err)
		}
	}

	if err := p.Store(ctx); err != nil {
		b.Fatal(err)
	}

	if l.Count != cacheSize {
		b.Fatal("item count in pool does not match expected size")
	}

	mask := len(keys) - 1
	start := time.Now()
	b.ResetTimer()

	b.RunParallel(func(pb *testing.PB) {
		index := int(rand.Uint32() & uint32(mask))

		for pb.Next() {
			_, err := p.GetRateLimit(ctx, &gubernator.RateLimitReq{
				CreatedAt: &createdAt,
				UniqueKey: keys[index&mask],
				Duration:  10_000,
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

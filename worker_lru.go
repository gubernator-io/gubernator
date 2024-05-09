package gubernator

import (
	"context"
	"fmt"
	"sync"
)

// WorkerHashiCache implements the InternalPool interface but has one cache. It is a singleton
// which all concurrent routines will send requests too.
type WorkerHashiCache struct {
	conf  Config
	cache Cache
}

// NewWorkerHashi creates a new singleton using the provided Config.CacheFactory
func NewWorkerHashi(conf Config) *WorkerHashiCache {
	return &WorkerHashiCache{
		cache: NewHashiCache(conf.CacheSize),
		conf:  conf,
	}
}

func (w *WorkerHashiCache) GetRateLimit(ctx context.Context, req *RateLimitReq, state RateLimitReqState) (*RateLimitResp, error) {
	var rlResponse *RateLimitResp
	var err error

	switch req.Algorithm {
	case Algorithm_TOKEN_BUCKET:
		rlResponse, err = tokenBucket(ctx, w.conf.Store, w.cache, req, state)
		if err != nil {
			msg := "Error in tokenBucket"
			countError(err, msg)

		}

	case Algorithm_LEAKY_BUCKET:
		rlResponse, err = leakyBucket(ctx, w.conf.Store, w.cache, req, state)
		if err != nil {
			msg := "Error in leakyBucket"
			countError(err, msg)

		}

	default:
		err = fmt.Errorf("invalid rate limit algorithm '%d'", req.Algorithm)

	}

	return rlResponse, err
}

func (w *WorkerHashiCache) Close() error {
	return nil
}

func (w *WorkerHashiCache) Store(ctx context.Context) error {
	out := make(chan *CacheItem, 500)
	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		for item := range w.cache.Each() {
			select {
			case out <- item:

			case <-ctx.Done():
				return
			}
		}
	}()

	// When all iterators are done, close `out` channel.
	go func() {
		wg.Wait()
		close(out)
	}()

	if ctx.Err() != nil {
		return ctx.Err()
	}

	if err := w.conf.Loader.Save(out); err != nil {
		return fmt.Errorf("while calling p.conf.Loader.Save(): %w", err)
	}
	return nil
}

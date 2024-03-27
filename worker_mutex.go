package gubernator

import (
	"context"
	"sync"

	"github.com/mailgun/errors"
	"github.com/prometheus/client_golang/prometheus"
	"go.opentelemetry.io/otel/trace"
)

// WorkerMutex implements the InternalPool interface but has one cache. It is a singleton
// which all concurrent routines will send requests too.
type WorkerMutex struct {
	mutex sync.Mutex
	conf  Config
	cache Cache
}

// NewWorkerMutex creates a new singleton using the provided Config.CacheFactory
func NewWorkerMutex(conf Config) *WorkerMutex {
	return &WorkerMutex{
		cache: conf.CacheFactory(conf.CacheSize),
		conf:  conf,
	}
}

func (w *WorkerMutex) GetRateLimit(ctx context.Context, req *RateLimitReq, state RateLimitReqState) (*RateLimitResp, error) {
	defer prometheus.NewTimer(metricFuncTimeDuration.WithLabelValues("WorkerMutex.GetRateLimit")).ObserveDuration()
	var rlResponse *RateLimitResp
	var err error

	w.mutex.Lock()
	defer w.mutex.Unlock()

	switch req.Algorithm {
	case Algorithm_TOKEN_BUCKET:
		rlResponse, err = tokenBucket(ctx, w.conf.Store, w.cache, req, state)
		if err != nil {
			msg := "Error in tokenBucket"
			countError(err, msg)
			err = errors.Wrap(err, msg)
			trace.SpanFromContext(ctx).RecordError(err)
		}

	case Algorithm_LEAKY_BUCKET:
		rlResponse, err = leakyBucket(ctx, w.conf.Store, w.cache, req, state)
		if err != nil {
			msg := "Error in leakyBucket"
			countError(err, msg)
			err = errors.Wrap(err, msg)
			trace.SpanFromContext(ctx).RecordError(err)
		}

	default:
		err = errors.Errorf("Invalid rate limit algorithm '%d'", req.Algorithm)
		trace.SpanFromContext(ctx).RecordError(err)
		metricCheckErrorCounter.WithLabelValues("Invalid algorithm").Add(1)
	}

	return rlResponse, err
}

func (w *WorkerMutex) Close() error {
	return nil
}

func (w *WorkerMutex) Store(ctx context.Context) error {
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
		return errors.Wrap(err, "while calling p.conf.Loader.Save()")
	}
	return nil
}

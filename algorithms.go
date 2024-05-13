/*
Copyright 2018-2022 Mailgun Technologies Inc

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

import (
	"context"
	"errors"

	"github.com/mailgun/holster/v4/clock"
	"github.com/prometheus/client_golang/prometheus"
	"go.opentelemetry.io/otel/trace"
)

var errAlreadyExistsInCache = errors.New("already exists in cache")

type rateContext struct {
	context.Context
	ReqState  RateLimitReqState
	Request   *RateLimitReq
	CacheItem *CacheItem
	Store     Store
	Cache     Cache
	// TODO: Remove
	InstanceID string
}

// ### NOTE ###
// The both token and leaky follow the same semantic which allows for requests of more than the limit
// to be rejected, but subsequent requests within the same window that are under the limit to succeed.
// IE: client attempts to send 1000 emails but 100 is their limit. The request is rejected as over the
// limit, but we do not set the remainder to 0 in the cache. The client can retry within the same window
// with 100 emails and the request will succeed. You can override this default behavior with `DRAIN_OVER_LIMIT`

// Implements token bucket algorithm for rate limiting. https://en.wikipedia.org/wiki/Token_bucket
func tokenBucket(ctx rateContext) (resp *RateLimitResp, err error) {
	tokenBucketTimer := prometheus.NewTimer(metricFuncTimeDuration.WithLabelValues("tokenBucket"))
	defer tokenBucketTimer.ObserveDuration()
	var ok bool
	// TODO: Remove
	//fmt.Printf("[%s] tokenBucket()\n", ctx.InstanceID)

	// Get rate limit from cache
	hashKey := ctx.Request.HashKey()
	ctx.CacheItem, ok = ctx.Cache.GetItem(hashKey)

	// If not in the cache, check the store if provided
	if ctx.Store != nil && !ok {
		if ctx.CacheItem, ok = ctx.Store.Get(ctx, ctx.Request); ok {
			if !ctx.Cache.Add(ctx.CacheItem) {
				// Someone else added a new token bucket item to the cache for this
				// rate limit before we did, so we retry by calling ourselves recursively.
				return tokenBucket(ctx)
			}
		}
	}

	// If no item was found, or the item is expired.
	if ctx.CacheItem == nil || ctx.CacheItem.IsExpired() {
		// Initialize the Token bucket item
		rl, err := InitTokenBucketItem(ctx)
		if err != nil && errors.Is(err, errAlreadyExistsInCache) {
			// Someone else added a new token bucket item to the cache for this
			// rate limit before we did, so we retry by calling ourselves recursively.
			return tokenBucket(ctx)
		}
		return rl, err
	}

	// Gain exclusive rights to this item while we calculate the rate limit
	ctx.CacheItem.mutex.Lock()
	defer ctx.CacheItem.mutex.Unlock()

	t, ok := ctx.CacheItem.Value.(*TokenBucketItem)
	if !ok {
		// Client switched algorithms; perhaps due to a migration?
		ctx.Cache.Remove(hashKey)
		if ctx.Store != nil {
			ctx.Store.Remove(ctx, hashKey)
		}
		ctx.CacheItem = nil

		rl, err := InitTokenBucketItem(ctx)
		if err != nil && errors.Is(err, errAlreadyExistsInCache) {
			return tokenBucket(ctx)
		}
		return rl, err
	}

	if HasBehavior(ctx.Request.Behavior, Behavior_RESET_REMAINING) {
		t.Remaining = ctx.Request.Limit
		t.Limit = ctx.Request.Limit
		t.Status = Status_UNDER_LIMIT

		if ctx.Store != nil {
			ctx.Store.OnChange(ctx, ctx.Request, ctx.CacheItem)
		}

		return &RateLimitResp{
			Status:    Status_UNDER_LIMIT,
			Limit:     ctx.Request.Limit,
			Remaining: ctx.Request.Limit,
			ResetTime: 0,
		}, nil
	}

	// Update the limit if it changed.
	if t.Limit != ctx.Request.Limit {
		// Add difference to remaining.
		t.Remaining += ctx.Request.Limit - t.Limit
		if t.Remaining < 0 {
			t.Remaining = 0
		}
		t.Limit = ctx.Request.Limit
	}

	rl := &RateLimitResp{
		Status:    t.Status,
		Limit:     ctx.Request.Limit,
		Remaining: t.Remaining,
		ResetTime: ctx.CacheItem.ExpireAt,
	}

	// If the duration config changed, update the new ExpireAt.
	if t.Duration != ctx.Request.Duration {
		span := trace.SpanFromContext(ctx)
		span.AddEvent("Duration changed")
		expire := t.CreatedAt + ctx.Request.Duration
		if HasBehavior(ctx.Request.Behavior, Behavior_DURATION_IS_GREGORIAN) {
			expire, err = GregorianExpiration(clock.Now(), ctx.Request.Duration)
			if err != nil {
				return nil, err
			}
		}

		// If our new duration means we are currently expired.
		createdAt := *ctx.Request.CreatedAt
		if expire <= createdAt {
			// Renew item.
			span.AddEvent("Limit has expired")
			expire = createdAt + ctx.Request.Duration
			t.CreatedAt = createdAt
			t.Remaining = t.Limit
		}

		ctx.CacheItem.ExpireAt = expire
		t.Duration = ctx.Request.Duration
		rl.ResetTime = expire
	}

	if ctx.Store != nil && ctx.ReqState.IsOwner {
		defer func() {
			ctx.Store.OnChange(ctx, ctx.Request, ctx.CacheItem)
		}()
	}

	// Client is only interested in retrieving the current status or
	// updating the rate limit config.
	if ctx.Request.Hits == 0 {
		return rl, nil
	}

	// If we are already at the limit.
	if rl.Remaining == 0 && ctx.Request.Hits > 0 {
		trace.SpanFromContext(ctx).AddEvent("Already over the limit")
		if ctx.ReqState.IsOwner {
			metricOverLimitCounter.Add(1)
		}
		rl.Status = Status_OVER_LIMIT
		t.Status = rl.Status
		return rl, nil
	}

	// If requested hits takes the remainder.
	if t.Remaining == ctx.Request.Hits {
		trace.SpanFromContext(ctx).AddEvent("At the limit")
		t.Remaining = 0
		rl.Remaining = 0
		return rl, nil
	}

	// If requested is more than available, then return over the limit
	// without updating the cache.
	if ctx.Request.Hits > t.Remaining {
		trace.SpanFromContext(ctx).AddEvent("Over the limit")
		if ctx.ReqState.IsOwner {
			metricOverLimitCounter.Add(1)
		}
		rl.Status = Status_OVER_LIMIT
		if HasBehavior(ctx.Request.Behavior, Behavior_DRAIN_OVER_LIMIT) {
			// DRAIN_OVER_LIMIT behavior drains the remaining counter.
			t.Remaining = 0
			rl.Remaining = 0
		}
		return rl, nil
	}

	t.Remaining -= ctx.Request.Hits
	rl.Remaining = t.Remaining
	return rl, nil
}

// InitTokenBucketItem will create a new item if the passed item is nil, else it will update the provided item.
func InitTokenBucketItem(ctx rateContext) (resp *RateLimitResp, err error) {
	createdAt := *ctx.Request.CreatedAt
	expire := createdAt + ctx.Request.Duration

	t := TokenBucketItem{
		Limit:     ctx.Request.Limit,
		Duration:  ctx.Request.Duration,
		Remaining: ctx.Request.Limit - ctx.Request.Hits,
		CreatedAt: createdAt,
	}

	// Add a new rate limit to the cache.
	if HasBehavior(ctx.Request.Behavior, Behavior_DURATION_IS_GREGORIAN) {
		expire, err = GregorianExpiration(clock.Now(), ctx.Request.Duration)
		if err != nil {
			return nil, err
		}
	}

	rl := &RateLimitResp{
		Status:    Status_UNDER_LIMIT,
		Limit:     ctx.Request.Limit,
		Remaining: t.Remaining,
		ResetTime: expire,
	}

	// Client could be requesting that we always return OVER_LIMIT.
	if ctx.Request.Hits > ctx.Request.Limit {
		trace.SpanFromContext(ctx).AddEvent("Over the limit")
		if ctx.ReqState.IsOwner {
			metricOverLimitCounter.Add(1)
		}
		rl.Status = Status_OVER_LIMIT
		rl.Remaining = ctx.Request.Limit
		t.Remaining = ctx.Request.Limit
	}

	// If the cache item already exists, update it
	if ctx.CacheItem != nil {
		ctx.CacheItem.mutex.Lock()
		ctx.CacheItem.Algorithm = Algorithm_TOKEN_BUCKET
		ctx.CacheItem.ExpireAt = expire
		in, ok := ctx.CacheItem.Value.(*TokenBucketItem)
		if !ok {
			// Likely the store gave us the wrong cache type
			ctx.CacheItem.mutex.Unlock()
			ctx.CacheItem = nil
			return InitTokenBucketItem(ctx)
		}
		*in = t
		ctx.CacheItem.mutex.Unlock()
	} else {
		// else create a new cache item and add it to the cache
		ctx.CacheItem = &CacheItem{
			Algorithm: Algorithm_TOKEN_BUCKET,
			Key:       ctx.Request.HashKey(),
			Value:     &t,
			ExpireAt:  expire,
		}
		if !ctx.Cache.Add(ctx.CacheItem) {
			return rl, errAlreadyExistsInCache
		}
	}

	if ctx.Store != nil && ctx.ReqState.IsOwner {
		ctx.Store.OnChange(ctx, ctx.Request, ctx.CacheItem)
	}

	return rl, nil
}

// Implements leaky bucket algorithm for rate limiting https://en.wikipedia.org/wiki/Leaky_bucket
func leakyBucket(ctx context.Context, s Store, c Cache, r *RateLimitReq, reqState RateLimitReqState) (resp *RateLimitResp, err error) {
	leakyBucketTimer := prometheus.NewTimer(metricFuncTimeDuration.WithLabelValues("V1Instance.getRateLimit_leakyBucket"))
	defer leakyBucketTimer.ObserveDuration()

	// TODO(thrawn01): Test for race conditions, and fix

	if r.Burst == 0 {
		r.Burst = r.Limit
	}

	createdAt := *r.CreatedAt

	// Get rate limit from cache.
	hashKey := r.HashKey()
	item, ok := c.GetItem(hashKey)

	if s != nil && !ok {
		// Cache missed, check our store for the item.
		if item, ok = s.Get(ctx, r); ok {
			if !c.Add(item) {
				// Someone else added a new leaky bucket item to the cache for this
				// rate limit before we did, so we retry by calling ourselves recursively.
				return leakyBucket(ctx, s, c, r, reqState)
			}
		}
	}

	if !ok {
		rl, err := leakyBucketNewItem(ctx, s, c, r, reqState)
		if err != nil && errors.Is(err, errAlreadyExistsInCache) {
			// Someone else added a new leaky bucket item to the cache for this
			// rate limit before we did, so we retry by calling ourselves recursively.
			return leakyBucket(ctx, s, c, r, reqState)
		}
		return rl, err
	}

	// Item found in cache or store.
	b, ok := item.Value.(*LeakyBucketItem)
	if !ok {
		// Client switched algorithms; perhaps due to a migration?
		c.Remove(hashKey)
		if s != nil {
			s.Remove(ctx, hashKey)
		}

		rl, err := leakyBucketNewItem(ctx, s, c, r, reqState)
		if err != nil && errors.Is(err, errAlreadyExistsInCache) {
			return leakyBucket(ctx, s, c, r, reqState)
		}
		return rl, err
	}

	// Gain exclusive rights to this item while we calculate the rate limit
	b.mutex.Lock()
	defer b.mutex.Unlock()

	if HasBehavior(r.Behavior, Behavior_RESET_REMAINING) {
		b.Remaining = float64(r.Burst)
	}

	// Update burst, limit and duration if they changed
	if b.Burst != r.Burst {
		if r.Burst > int64(b.Remaining) {
			b.Remaining = float64(r.Burst)
		}
		b.Burst = r.Burst
	}

	b.Limit = r.Limit
	b.Duration = r.Duration

	duration := r.Duration
	rate := float64(duration) / float64(r.Limit)

	if HasBehavior(r.Behavior, Behavior_DURATION_IS_GREGORIAN) {
		d, err := GregorianDuration(clock.Now(), r.Duration)
		if err != nil {
			return nil, err
		}
		n := clock.Now()
		expire, err := GregorianExpiration(n, r.Duration)
		if err != nil {
			return nil, err
		}

		// Calculate the rate using the entire duration of the gregorian interval
		// IE: Minute = 60,000 milliseconds, etc.. etc..
		rate = float64(d) / float64(r.Limit)
		// Update the duration to be the end of the gregorian interval
		duration = expire - (n.UnixNano() / 1000000)
	}

	if r.Hits != 0 {
		c.UpdateExpiration(r.HashKey(), createdAt+duration)
	}

	// Calculate how much leaked out of the bucket since the last time we leaked a hit
	elapsed := createdAt - b.UpdatedAt
	leak := float64(elapsed) / rate

	if int64(leak) > 0 {
		b.Remaining += leak
		b.UpdatedAt = createdAt
	}

	if int64(b.Remaining) > b.Burst {
		b.Remaining = float64(b.Burst)
	}

	rl := &RateLimitResp{
		Limit:     b.Limit,
		Remaining: int64(b.Remaining),
		Status:    Status_UNDER_LIMIT,
		ResetTime: createdAt + (b.Limit-int64(b.Remaining))*int64(rate),
	}

	// TODO: Feature missing: check for Duration change between item/request.

	if s != nil && reqState.IsOwner {
		defer func() {
			s.OnChange(ctx, r, item)
		}()
	}

	// If we are already at the limit
	if int64(b.Remaining) == 0 && r.Hits > 0 {
		if reqState.IsOwner {
			metricOverLimitCounter.Add(1)
		}
		rl.Status = Status_OVER_LIMIT
		return rl, nil
	}

	// If requested hits takes the remainder
	if int64(b.Remaining) == r.Hits {
		b.Remaining = 0
		rl.Remaining = int64(b.Remaining)
		rl.ResetTime = createdAt + (rl.Limit-rl.Remaining)*int64(rate)
		return rl, nil
	}

	// If requested is more than available, then return over the limit
	// without updating the bucket, unless `DRAIN_OVER_LIMIT` is set.
	if r.Hits > int64(b.Remaining) {
		if reqState.IsOwner {
			metricOverLimitCounter.Add(1)
		}
		rl.Status = Status_OVER_LIMIT

		// DRAIN_OVER_LIMIT behavior drains the remaining counter.
		if HasBehavior(r.Behavior, Behavior_DRAIN_OVER_LIMIT) {
			b.Remaining = 0
			rl.Remaining = 0
		}

		return rl, nil
	}

	// Client is only interested in retrieving the current status
	if r.Hits == 0 {
		return rl, nil
	}

	b.Remaining -= float64(r.Hits)
	rl.Remaining = int64(b.Remaining)
	rl.ResetTime = createdAt + (rl.Limit-rl.Remaining)*int64(rate)
	return rl, nil

}

// Called by leakyBucket() when adding a new item in the store.
func leakyBucketNewItem(ctx context.Context, s Store, c Cache, r *RateLimitReq, reqState RateLimitReqState) (resp *RateLimitResp, err error) {
	createdAt := *r.CreatedAt
	duration := r.Duration
	rate := float64(duration) / float64(r.Limit)
	if HasBehavior(r.Behavior, Behavior_DURATION_IS_GREGORIAN) {
		n := clock.Now()
		expire, err := GregorianExpiration(n, r.Duration)
		if err != nil {
			return nil, err
		}
		// Set the initial duration as the remainder of time until
		// the end of the gregorian interval.
		duration = expire - (n.UnixNano() / 1000000)
	}

	// Create a new leaky bucket
	b := LeakyBucketItem{
		Remaining: float64(r.Burst - r.Hits),
		Limit:     r.Limit,
		Duration:  duration,
		UpdatedAt: createdAt,
		Burst:     r.Burst,
	}

	rl := RateLimitResp{
		Status:    Status_UNDER_LIMIT,
		Limit:     b.Limit,
		Remaining: r.Burst - r.Hits,
		ResetTime: createdAt + (b.Limit-(r.Burst-r.Hits))*int64(rate),
	}

	// Client could be requesting that we start with the bucket OVER_LIMIT
	if r.Hits > r.Burst {
		if reqState.IsOwner {
			metricOverLimitCounter.Add(1)
		}
		rl.Status = Status_OVER_LIMIT
		rl.Remaining = 0
		rl.ResetTime = createdAt + (rl.Limit-rl.Remaining)*int64(rate)
		b.Remaining = 0
	}

	item := &CacheItem{
		ExpireAt:  createdAt + duration,
		Algorithm: r.Algorithm,
		Key:       r.HashKey(),
		Value:     &b,
	}

	if !c.Add(item) {
		return nil, errAlreadyExistsInCache
	}

	if s != nil && reqState.IsOwner {
		s.OnChange(ctx, r, item)
	}

	return &rl, nil
}

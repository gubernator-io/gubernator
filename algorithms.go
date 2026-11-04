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
	"math"

	"github.com/mailgun/holster/v4/clock"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/sirupsen/logrus"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// ### NOTE ###
// The both token and leaky follow the same semantic which allows for requests of more than the limit
// to be rejected, but subsequent requests within the same window that are under the limit to succeed.
// IE: client attempts to send 1000 emails but 100 is their limit. The request is rejected as over the
// limit, but we do not set the remainder to 0 in the cache. The client can retry within the same window
// with 100 emails and the request will succeed. You can override this default behavior with `DRAIN_OVER_LIMIT`

// Implements token bucket algorithm for rate limiting. https://en.wikipedia.org/wiki/Token_bucket
func tokenBucket(ctx context.Context, s Store, c Cache, r *RateLimitReq, reqState RateLimitReqState) (resp *RateLimitResp, err error) {
	tokenBucketTimer := prometheus.NewTimer(metricFuncTimeDuration.WithLabelValues("tokenBucket"))
	defer tokenBucketTimer.ObserveDuration()

	// Get rate limit from cache.
	hashKey := r.HashKey()
	item, ok := c.GetItem(hashKey)

	if s != nil && !ok {
		// Cache miss.
		// Check our store for the item.
		if item, ok = s.Get(ctx, r); ok {
			c.Add(item)
		}
	}

	// Sanity checks.
	if ok {
		if item.Value == nil {
			msgPart := "tokenBucket: Invalid cache item; Value is nil"
			trace.SpanFromContext(ctx).AddEvent("log", trace.WithAttributes(
				attribute.String("log.message", msgPart),
				attribute.String("log.severity", "WARNING"),
				attribute.String("hashKey", hashKey),
				attribute.String("key", r.UniqueKey),
				attribute.String("name", r.Name),
			))
			logrus.Error(msgPart)
			ok = false
		} else if item.Key != hashKey {
			msgPart := "tokenBucket: Invalid cache item; key mismatch"
			trace.SpanFromContext(ctx).AddEvent("log", trace.WithAttributes(
				attribute.String("log.message", msgPart),
				attribute.String("log.severity", "WARNING"),
				attribute.String("itemKey", item.Key),
				attribute.String("hashKey", hashKey),
				attribute.String("name", r.Name),
			))
			logrus.Error(msgPart)
			ok = false
		}
	}

	if ok {
		// Item found in cache or store.
		// Both removals below are documented resets, so booked debt on a leaky item is forgiven and counted.
		if lb, leaky := item.Value.(*LeakyBucketItem); leaky && debtAt(lb, epochMillis(clock.Now())) {
			metricScheduledDebtResetCount.Add(1)
		}
		if HasBehavior(r.Behavior, Behavior_RESET_REMAINING) {
			c.Remove(hashKey)

			if s != nil {
				s.Remove(ctx, hashKey)
			}
			return &RateLimitResp{
				Status:    Status_UNDER_LIMIT,
				Limit:     r.Limit,
				Remaining: r.Limit,
				ResetTime: 0,
			}, nil
		}
		t, ok := item.Value.(*TokenBucketItem)
		if !ok {
			// Client switched algorithms; perhaps due to a migration?
			c.Remove(hashKey)

			if s != nil {
				s.Remove(ctx, hashKey)
			}

			return tokenBucketNewItem(ctx, s, c, r, reqState)
		}

		// Update the limit if it changed.
		if t.Limit != r.Limit {
			// Add difference to remaining.
			t.Remaining += r.Limit - t.Limit
			if t.Remaining < 0 {
				t.Remaining = 0
			}
			t.Limit = r.Limit
		}

		rl := &RateLimitResp{
			Status:    Status_UNDER_LIMIT,
			Limit:     r.Limit,
			Remaining: t.Remaining,
			ResetTime: item.ExpireAt,
		}

		// If the duration config changed, update the new ExpireAt.
		if t.Duration != r.Duration {
			expire := t.CreatedAt + r.Duration
			if HasBehavior(r.Behavior, Behavior_DURATION_IS_GREGORIAN) {
				expire, err = GregorianExpiration(clock.Now(), r.Duration)
				if err != nil {
					return nil, err
				}
			}

			// If our new duration means we are currently expired.
			createdAt := *r.CreatedAt
			if expire <= createdAt {
				// Renew item.
				expire = createdAt + r.Duration
				t.CreatedAt = createdAt
				t.Remaining = t.Limit
			}

			item.ExpireAt = expire
			t.Duration = r.Duration
			rl.ResetTime = expire
			rl.Remaining = t.Remaining
		}

		if s != nil && reqState.IsOwner {
			defer func() {
				s.OnChange(ctx, r, item)
			}()
		}

		// Client is only interested in retrieving the current status or
		// updating the rate limit config.
		if r.Hits == 0 {
			// t.Status is the last admit/refuse decision. It only applies while
			// the bucket is empty; a limit increase or renewal makes it stale,
			// so clear it or it resurfaces when the bucket empties again.
			if rl.Remaining == 0 {
				rl.Status = t.Status
			} else {
				t.Status = Status_UNDER_LIMIT
			}
			return rl, nil
		}

		// If we are already at the limit.
		if rl.Remaining == 0 && r.Hits > 0 {
			if reqState.IsOwner {
				metricOverLimitCounter.Add(1)
			}
			rl.Status = Status_OVER_LIMIT
			t.Status = rl.Status
			return rl, nil
		}

		// Clears a stale OVER_LIMIT. Oversized refusals below leave it
		// UNDER_LIMIT, which TestDrainOverLimit pins for hits=0 queries.
		t.Status = Status_UNDER_LIMIT

		// If requested hits takes the remainder.
		if t.Remaining == r.Hits {
			t.Remaining = 0
			rl.Remaining = 0
			return rl, nil
		}

		// If requested is more than available, then return over the limit
		// without updating the cache.
		if r.Hits > t.Remaining {
			if reqState.IsOwner {
				metricOverLimitCounter.Add(1)
			}
			rl.Status = Status_OVER_LIMIT
			if HasBehavior(r.Behavior, Behavior_DRAIN_OVER_LIMIT) {
				// DRAIN_OVER_LIMIT behavior drains the remaining counter.
				t.Remaining = 0
				rl.Remaining = 0
			}
			return rl, nil
		}

		t.Remaining -= r.Hits
		rl.Remaining = t.Remaining
		return rl, nil
	}

	// Item is not found in cache or store, create new.
	return tokenBucketNewItem(ctx, s, c, r, reqState)
}

// Called by tokenBucket() when adding a new item in the store.
func tokenBucketNewItem(ctx context.Context, s Store, c Cache, r *RateLimitReq, reqState RateLimitReqState) (resp *RateLimitResp, err error) {
	createdAt := *r.CreatedAt
	expire := createdAt + r.Duration

	t := &TokenBucketItem{
		Limit:     r.Limit,
		Duration:  r.Duration,
		Remaining: r.Limit - r.Hits,
		CreatedAt: createdAt,
	}

	// Add a new rate limit to the cache.
	if HasBehavior(r.Behavior, Behavior_DURATION_IS_GREGORIAN) {
		expire, err = GregorianExpiration(clock.Now(), r.Duration)
		if err != nil {
			return nil, err
		}
	}

	item := &CacheItem{
		Algorithm: Algorithm_TOKEN_BUCKET,
		Key:       r.HashKey(),
		Value:     t,
		ExpireAt:  expire,
	}

	rl := &RateLimitResp{
		Status:    Status_UNDER_LIMIT,
		Limit:     r.Limit,
		Remaining: t.Remaining,
		ResetTime: expire,
	}

	// Client could be requesting that we always return OVER_LIMIT.
	if r.Hits > r.Limit {
		if reqState.IsOwner {
			metricOverLimitCounter.Add(1)
		}
		rl.Status = Status_OVER_LIMIT
		rl.Remaining = r.Limit
		t.Remaining = r.Limit
	}

	c.Add(item)

	if s != nil && reqState.IsOwner {
		s.OnChange(ctx, r, item)
	}

	return rl, nil
}

// Implements leaky bucket algorithm for rate limiting https://en.wikipedia.org/wiki/Leaky_bucket
func leakyBucket(ctx context.Context, s Store, c Cache, r *RateLimitReq, reqState RateLimitReqState) (resp *RateLimitResp, err error) {
	leakyBucketTimer := prometheus.NewTimer(metricFuncTimeDuration.WithLabelValues("V1Instance.getRateLimit_leakyBucket"))
	defer leakyBucketTimer.ObserveDuration()

	if r.Burst == 0 {
		r.Burst = r.Limit
	}

	createdAt := *r.CreatedAt

	// Get rate limit from cache.
	hashKey := r.HashKey()
	item, ok := c.GetItem(hashKey)

	if s != nil && !ok {
		// Cache miss.
		// Check our store for the item.
		if item, ok = s.Get(ctx, r); ok {
			c.Add(item)
		}
	}

	// Sanity checks.
	if ok {
		if item.Value == nil {
			msgPart := "leakyBucket: Invalid cache item; Value is nil"
			trace.SpanFromContext(ctx).AddEvent("log", trace.WithAttributes(
				attribute.String("log.message", msgPart),
				attribute.String("log.severity", "WARNING"),
				attribute.String("hashKey", hashKey),
				attribute.String("key", r.UniqueKey),
				attribute.String("name", r.Name),
			))
			logrus.Error(msgPart)
			ok = false
		} else if item.Key != hashKey {
			msgPart := "leakyBucket: Invalid cache item; key mismatch"
			trace.SpanFromContext(ctx).AddEvent("log", trace.WithAttributes(
				attribute.String("log.message", msgPart),
				attribute.String("log.severity", "WARNING"),
				attribute.String("itemKey", item.Key),
				attribute.String("hashKey", hashKey),
				attribute.String("name", r.Name),
			))
			logrus.Error(msgPart)
			ok = false
		}
	}

	if ok {
		// Item found in cache or store.

		b, ok := item.Value.(*LeakyBucketItem)
		if !ok {
			// Client switched algorithms; perhaps due to a migration?
			c.Remove(hashKey)

			if s != nil {
				s.Remove(ctx, hashKey)
			}

			return leakyBucketNewItem(ctx, s, c, r, reqState)
		}

		// A key in debt and any max_wait request are timed by the owner's clock, so a
		// client-stamped created_at cannot move a booking.
		if r.MaxWait > 0 || b.Remaining < 0 {
			createdAt = epochMillis(clock.Now())
		}

		duration := r.Duration
		rate := float64(duration) / float64(r.Limit)

		// Debt is booked at the stored rate. A changed limit or duration rescales it to the new
		// rate so the debt clears at the same moment and no granted slot is freed or pushed out.
		// A Gregorian rate follows the calendar and an unusable one cannot be credited, so such a
		// request leaves the stored config alone while the key is in debt. A request's limit and
		// duration are checked one by one, because two negatives make a positive rate. The stored
		// quotient is the rate the debt was booked at, whatever the signs.
		gregorian := HasBehavior(r.Behavior, Behavior_DURATION_IS_GREGORIAN)
		storedRate := float64(b.Duration) / float64(b.Limit)
		storedUsable := !unusableRate(storedRate)
		requestUsable := r.Limit > 0 && r.Duration > 0 && !unusableRate(rate)
		configChanged := b.Limit != r.Limit || b.Duration != r.Duration
		keepStored := (gregorian || !requestUsable) && b.Remaining < 0 && storedUsable
		rescale := !gregorian && requestUsable && b.Remaining < 0 && configChanged && storedUsable
		// A burst of zero or less, which a limit of zero or less produces when burst is omitted, would
		// deepen the debt through the clamp below or refill it later, so a key still in debt after
		// this request's leak is credited keeps its own.
		burst := r.Burst
		if burst <= 0 && debtAt(b, createdAt) {
			burst = b.Burst
		}

		if gregorian {
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
		requestRate := rate
		if keepStored {
			rate = storedRate
		}

		// wasInDebt is the balance before this request's leak is credited, so it can be true
		// for a request that finds the debt already paid. Expiry of such a key is owned by
		// extendDebtExpiry, which only lengthens it, so today's now+duration update is skipped.
		wasInDebt := b.Remaining < 0 && !HasBehavior(r.Behavior, Behavior_RESET_REMAINING)
		burstChanged := b.Burst != burst
		if r.Hits != 0 && !wasInDebt {
			c.UpdateExpiration(r.HashKey(), createdAt+duration)
		}

		// Credit elapsed time as fractional tokens on every request. Advancing
		// UpdatedAt only after a whole token leaked drops or double counts the
		// fraction, and clamping through int64 lets 3.0x pass burst 3.
		creditRate := rate
		if rescale {
			creditRate = storedRate
		}
		if createdAt > b.UpdatedAt {
			b.Remaining += creditedLeak(b, createdAt, creditRate)
			b.UpdatedAt = createdAt
		}
		if rescale && snapToZero(b.Remaining) < 0 {
			b.Remaining *= storedRate / rate
		}

		// Remaining is the unspent balance and Burst is the capacity, so a Burst change
		// shifts the balance by the same amount. It runs after the leak credit so the
		// floor at zero cannot swallow part of a decrease that the credit then refills.
		// A debt still owed is left alone, since a burst change must not forgive it.
		if burstChanged {
			if snapToZero(b.Remaining) >= 0 {
				b.Remaining = math.Max(0, b.Remaining+float64(burst-b.Burst))
			}
			b.Burst = burst
		}

		// After the Burst shift, so an explicit refill always lands on the new Burst.
		if HasBehavior(r.Behavior, Behavior_RESET_REMAINING) {
			if snapToZero(b.Remaining) < 0 {
				metricScheduledDebtResetCount.Add(1)
			}
			b.Remaining = float64(burst)
		}

		// Clamp after the leak so credit never lifts Remaining above Burst.
		b.Remaining = snapToZero(math.Min(float64(b.Burst), b.Remaining))

		// Decided on the final balance, so a request that repays or resets the debt adopts its config.
		if !keepStored || b.Remaining >= 0 {
			b.Limit = r.Limit
			b.Duration = r.Duration
			rate = requestRate
		}

		rl := &RateLimitResp{
			Limit:     b.Limit,
			Remaining: spendableTokens(b.Remaining),
			Status:    Status_UNDER_LIMIT,
			ResetTime: createdAt + (b.Limit-wholeTokens(b.Remaining))*int64(rate),
		}
		if b.Remaining < 0 {
			rl.ResetTime = fullAt(b, rate)
		}

		// TODO: Feature missing: check for Duration change between item/request.

		if s != nil && reqState.IsOwner {
			defer func() {
				s.OnChange(ctx, r, item)
			}()
		}
		if wasInDebt && (burstChanged || configChanged) && !unusableRate(rate) {
			// Deferred after OnChange so it runs first and the Store sees the extended expiry.
			// A new burst, limit or duration moves the moment the balance is back at burst, whatever the outcome.
			defer extendDebtExpiry(c, item, b, createdAt, duration, rate)
		}

		// Client is only interested in retrieving the current status
		if r.Hits == 0 {
			return rl, nil
		}

		if r.MaxWait > 0 {
			return scheduleHits(c, item, b, r, reqState, rl, createdAt, duration, rate, wasInDebt), nil
		}

		// Negative hits refund tokens, but never pay off booked debt.
		if r.Hits < 0 && b.Remaining < 0 {
			extendDebtExpiry(c, item, b, createdAt, duration, rate)
			return rl, nil
		}

		// If requested is more than available, then return over the limit
		// without updating the bucket, unless `DRAIN_OVER_LIMIT` is set.
		if r.Hits > wholeTokens(b.Remaining) {
			if reqState.IsOwner {
				metricOverLimitCounter.Add(1)
			}
			rl.Status = Status_OVER_LIMIT

			// DRAIN_OVER_LIMIT behavior drains the remaining counter, but never
			// raises debt to zero.
			if HasBehavior(r.Behavior, Behavior_DRAIN_OVER_LIMIT) {
				b.Remaining = math.Min(b.Remaining, 0)
				rl.Remaining = 0
			}

			return rl, nil
		}

		// Negative hits refund tokens, which must not lift the balance above burst.
		b.Remaining = snapToZero(math.Min(float64(b.Burst), b.Remaining-float64(r.Hits)))
		rl.Remaining = wholeTokens(b.Remaining)
		rl.ResetTime = createdAt + (rl.Limit-rl.Remaining)*int64(rate)
		if wasInDebt {
			extendDebtExpiry(c, item, b, createdAt, duration, rate)
		}
		return rl, nil
	}

	return leakyBucketNewItem(ctx, s, c, r, reqState)
}

// creditedLeak is the tokens that leaked into b between its last credit and now. A NaN
// rate (Limit and Duration both 0) must not poison the stored balance.
func creditedLeak(b *LeakyBucketItem, now int64, rate float64) float64 {
	if now <= b.UpdatedAt {
		return 0
	}
	if leak := float64(now-b.UpdatedAt) / rate; !math.IsNaN(leak) {
		return leak
	}
	return 0
}

// debtAt reports whether b's balance, credited to now, is still below zero.
func debtAt(b *LeakyBucketItem, now int64) bool {
	balance := b.Remaining
	if rate := float64(b.Duration) / float64(b.Limit); !unusableRate(rate) {
		balance += creditedLeak(b, now, rate)
	}
	return snapToZero(balance) < 0
}

// fullAt is when b's balance is back at burst, in epoch milliseconds.
func fullAt(b *LeakyBucketItem, rate float64) int64 {
	return millis(math.Ceil(float64(b.UpdatedAt) + (float64(b.Burst)-b.Remaining)*rate))
}

// millis converts to int64, saturating where Go leaves the conversion of an out-of-range float
// implementation-defined (amd64 gives MinInt64, arm64 saturates).
func millis(f float64) int64 {
	switch {
	case f >= math.MaxInt64:
		return math.MaxInt64
	case f <= math.MinInt64:
		return math.MinInt64
	}
	return int64(f)
}

// unusableRate is a rate no wait can be computed from: a limit or duration that is zero or
// negative, which makes the rate infinite, NaN or negative.
func unusableRate(rate float64) bool {
	return !(rate > 0) || math.IsInf(rate, 0)
}

// spendableTokens is the balance as a response's remaining: whole tokens, never negative.
func spendableTokens(balance float64) int64 {
	return max(0, wholeTokens(balance))
}

// extendDebtExpiry keeps a key in debt cached until its balance is back at burst.
// Expiring earlier would hand the next caller a full bucket and forget the bookings.
func extendDebtExpiry(c Cache, item *CacheItem, b *LeakyBucketItem, now, duration int64, rate float64) {
	c.UpdateExpiration(item.Key, max(item.ExpireAt, now+duration, fullAt(b, rate)))
}

// scheduleHits answers a max_wait request for a positive number of hits: it admits them,
// books them as debt when they fit within the wait allowed, or refuses and charges nothing.
func scheduleHits(c Cache, item *CacheItem, b *LeakyBucketItem, r *RateLimitReq, reqState RateLimitReqState,
	rl *RateLimitResp, now, duration int64, rate float64, wasInDebt bool) *RateLimitResp {

	if r.Hits > b.Burst || unusableRate(rate) {
		if reqState.IsOwner {
			metricOverLimitCounter.Add(1)
		}
		rl.Status = Status_NEVER_FITS
		return rl
	}

	if r.Hits <= wholeTokens(b.Remaining) {
		b.Remaining = snapToZero(b.Remaining - float64(r.Hits))
		rl.Remaining = spendableTokens(b.Remaining)
		rl.ResetTime = now + (rl.Limit-rl.Remaining)*int64(rate)
		if wasInDebt {
			extendDebtExpiry(c, item, b, now, duration, rate)
		}
		return rl
	}

	// UpdatedAt lies ahead of now only when an earlier plain request on a key not in debt carried a future created_at.
	// The 1e-6 keeps float error from rounding an exact wait such as 6000 up to 6001.
	wait := max(1, millis(math.Ceil(float64(max(b.UpdatedAt, now)-now)+(float64(r.Hits)-b.Remaining)*rate-1e-6)))
	ceiling := reqState.WaitCeiling.Milliseconds()
	if wait > min(r.MaxWait, ceiling) {
		if reqState.IsOwner {
			metricOverLimitCounter.Add(1)
			if wait > ceiling && wait <= r.MaxWait {
				metricScheduledCeilingRefusedCount.Add(1)
			}
		}
		rl.Status = Status_OVER_LIMIT
		rl.RetryAfter = wait
		return rl
	}

	b.Remaining -= float64(r.Hits)
	rl.Status = Status_SCHEDULED
	rl.RetryAfter = wait
	rl.Remaining = 0
	rl.ResetTime = fullAt(b, rate)
	if reqState.IsOwner {
		metricScheduledCount.Add(1)
	}
	extendDebtExpiry(c, item, b, now, duration, rate)
	return rl
}

// snapToZero absorbs float error, such as a balance of 2.9999999999 paying for 3 hits or a debt
// that leak repays to -9e-16.
// A balance below zero means debt everywhere else, so a bucket spent exactly to zero must not read as in debt.
func snapToZero(balance float64) float64 {
	if math.Abs(balance) < 1e-9 {
		return 0
	}
	return balance
}

// wholeTokens converts a fractional balance to the tokens a caller can spend.
// The epsilon absorbs float error so a rate like 10000/14 that leaks exactly 7
// tokens in 5s reads 7 rather than 6.999999999999999.
func wholeTokens(balance float64) int64 {
	return int64(balance + 1e-9)
}

// Called by leakyBucket() when adding a new item in the store.
func leakyBucketNewItem(ctx context.Context, s Store, c Cache, r *RateLimitReq, reqState RateLimitReqState) (resp *RateLimitResp, err error) {
	createdAt := *r.CreatedAt
	if r.MaxWait > 0 {
		createdAt = epochMillis(clock.Now())
	}
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

	// A request that can never fit creates no bucket, so the key stays full for the next caller.
	if r.MaxWait > 0 && r.Hits > r.Burst {
		if reqState.IsOwner {
			metricOverLimitCounter.Add(1)
		}
		return &RateLimitResp{
			Status:    Status_NEVER_FITS,
			Limit:     r.Limit,
			Remaining: r.Burst,
			ResetTime: createdAt + (r.Limit-r.Burst)*int64(rate),
		}, nil
	}

	// Create a new leaky bucket
	remaining := min(r.Burst, r.Burst-r.Hits)
	b := LeakyBucketItem{
		Remaining: float64(remaining),
		Limit:     r.Limit,
		Duration:  duration,
		UpdatedAt: createdAt,
		Burst:     r.Burst,
	}

	rl := RateLimitResp{
		Status:    Status_UNDER_LIMIT,
		Limit:     b.Limit,
		Remaining: remaining,
		ResetTime: createdAt + (b.Limit-remaining)*int64(rate),
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

	c.Add(item)

	if s != nil && reqState.IsOwner {
		s.OnChange(ctx, r, item)
	}

	return &rl, nil
}

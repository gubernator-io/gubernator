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
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/kapetan-io/tackle/wait"

	"github.com/duh-rpc/duh-go"
	v1 "github.com/duh-rpc/duh-go/proto/v1"
	"github.com/gubernator-io/gubernator/v3/tracing"
	"github.com/kapetan-io/tackle/clock"
	"github.com/prometheus/client_golang/prometheus"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/protobuf/proto"
)

const (
	maxBatchSize = 1000
	Healthy      = "healthy"
	UnHealthy    = "unhealthy"
)

type Service struct {
	propagator propagation.TraceContext
	global     *globalManager
	peerMutex  sync.RWMutex
	cache      CacheManager
	log        FieldLogger
	conf       Config
	isClosed   bool
}

// RateLimitContext is context that is not included in the RateLimitRequest but is needed by algorithms.go
type RateLimitContext struct {
	IsOwner bool
}

var (
	metricGetRateLimitCounter = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "gubernator_getratelimit_counter",
		Help: "The count of checkLocalRateLimit() calls.  Label \"calltype\" may be \"local\" for calls handled by the same peer, or \"global\" for global rate limits.",
	}, []string{"calltype"})
	metricFuncTimeDuration = prometheus.NewSummaryVec(prometheus.SummaryOpts{
		Name: "gubernator_func_duration",
		Help: "The timings of key functions in Gubernator in seconds.",
		Objectives: map[float64]float64{
			1:    0.001,
			0.99: 0.001,
			0.5:  0.01,
		},
	}, []string{"name"})
	metricOverLimitCounter = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "gubernator_over_limit_counter",
		Help: "The number of rate limit checks that are over the limit.",
	})
	metricConcurrentChecks = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "gubernator_concurrent_checks_counter",
		Help: "The number of concurrent GetRateLimits API calls.",
	})
	metricCheckErrorCounter = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "gubernator_check_error_counter",
		Help: "The number of errors while checking rate limits.",
	}, []string{"error"})

	// Batch behavior.
	metricBatchSendRetries = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "gubernator_batch_send_retries",
		Help: "The count of retries occurred in asyncRequest() forwarding a request to another peer.",
	}, []string{"name"})
	metricBatchQueueLength = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "gubernator_batch_queue_length",
		Help: "The getRateLimitsBatch() queue length in PeerClient.  This represents rate checks queued by for batching to a remote peer.",
	}, []string{"peerAddr"})
	metricBatchSendDuration = prometheus.NewSummaryVec(prometheus.SummaryOpts{
		Name: "gubernator_batch_send_duration",
		Help: "The timings of batch send operations to a remote peer.",
		Objectives: map[float64]float64{
			0.99: 0.001,
		},
	}, []string{"peerAddr"})
)

// NewService instantiate a single instance of a gubernator service
func NewService(conf Config) (s *Service, err error) {
	ctx := context.Background()

	if err := conf.SetDefaults(); err != nil {
		return nil, err
	}

	s = &Service{
		log:  conf.Logger,
		conf: conf,
	}

	s.cache, err = NewCacheManager(conf)
	if err != nil {
		return nil, fmt.Errorf("during NewCacheManager(): %w", err)
	}
	s.global = newGlobalManager(conf.Behaviors, s)

	if s.conf.Loader == nil {
		return s, nil
	}

	// Load the cache.
	err = s.cache.Load(ctx)
	if err != nil {
		return nil, fmt.Errorf("error in CacheManager.Load: %w", err)
	}

	return s, nil
}

func (s *Service) Close(ctx context.Context) (err error) {
	if s.isClosed {
		return nil
	}

	s.global.Close()

	if s.conf.Loader != nil {
		err = s.cache.Store(ctx)
		if err != nil {
			s.log.LogAttrs(context.TODO(), slog.LevelError, "Error in workerPool.Store",
				ErrAttr(err),
			)
			return fmt.Errorf("error in CacheManager.Store: %w", err)
		}
	}

	err = s.cache.Close()
	if err != nil {
		s.log.LogAttrs(context.TODO(), slog.LevelError, "Error in workerPool.Close",
			ErrAttr(err),
		)
		return fmt.Errorf("error in CacheManager.Close: %w", err)
	}

	// Close all the peer clients
	s.SetPeers([]PeerInfo{})

	s.isClosed = true
	return nil
}

// CheckRateLimits is the public interface used by clients to request rate limits from the system. If the
// rate limit `Name` and `UniqueKey` is not owned by this instance, then we forward the request to the
// peer that does.
func (s *Service) CheckRateLimits(ctx context.Context, req *CheckRateLimitsRequest, resp *CheckRateLimitsResponse) (err error) {
	funcTimer := prometheus.NewTimer(metricFuncTimeDuration.WithLabelValues("Service.CheckRateLimits"))
	defer funcTimer.ObserveDuration()
	metricConcurrentChecks.Inc()
	defer metricConcurrentChecks.Dec()

	if len(req.Requests) > maxBatchSize {
		metricCheckErrorCounter.WithLabelValues("Request too large").Add(1)
		return duh.NewServiceError(duh.CodeBadRequest,
			fmt.Sprintf("CheckRateLimitsRequest.RateLimits list too large; max size is '%d'", maxBatchSize), nil, nil)
	}

	if len(req.Requests) == 0 {
		return duh.NewServiceError(duh.CodeBadRequest,
			"CheckRateLimitsRequest.RateLimits list is empty; provide at least one rate limit", nil, nil)
	}

	resp.Responses = make([]*RateLimitResponse, len(req.Requests))
	asyncCh := make(chan AsyncResp, len(req.Requests))
	createdAt := epochMillis(clock.Now())
	var wg sync.WaitGroup

	// For each item in the request body
	for i, r := range req.Requests {
		key := r.Name + "_" + r.UniqueKey
		var peer *Peer
		var err error

		if r.UniqueKey == "" {
			metricCheckErrorCounter.WithLabelValues("Invalid request").Inc()
			resp.Responses[i] = &RateLimitResponse{Error: "field 'unique_key' cannot be empty"}
			continue
		}

		if r.Name == "" {
			metricCheckErrorCounter.WithLabelValues("Invalid request").Inc()
			resp.Responses[i] = &RateLimitResponse{Error: "field 'namespace' cannot be empty"}
			continue
		}

		if r.CreatedAt == nil || *r.CreatedAt == 0 {
			r.CreatedAt = &createdAt
		}

		if ctx.Err() != nil {
			err = fmt.Errorf("error while iterating request items: %w", ctx.Err())
			span := trace.SpanFromContext(ctx)
			span.RecordError(err)
			resp.Responses[i] = &RateLimitResponse{
				Error: err.Error(),
			}
			continue
		}

		if s.conf.Behaviors.ForceGlobal {
			SetBehavior(&r.Behavior, Behavior_GLOBAL, true)
		}

		peer, err = s.GetPeer(ctx, key)
		if err != nil {
			countError(err, "Error in GetPeer")
			err = fmt.Errorf("error in GetPeer, looking up peer that owns rate limit '%s': %w", key, err)
			resp.Responses[i] = &RateLimitResponse{
				Error: err.Error(),
			}
			continue
		}

		// If our server instance is the owner of this rate limit
		reqState := RateLimitContext{IsOwner: peer.Info().IsOwner}
		if reqState.IsOwner {
			// Apply our rate limit algorithm to the request
			resp.Responses[i], err = s.checkLocalRateLimit(ctx, r, reqState)
			if err != nil {
				err = fmt.Errorf("error while apply rate limit for '%s': %w", key, err)
				span := trace.SpanFromContext(ctx)
				span.RecordError(err)
				resp.Responses[i] = &RateLimitResponse{Error: err.Error()}
			}
		} else {
			if HasBehavior(r.Behavior, Behavior_GLOBAL) {
				resp.Responses[i], err = s.checkGlobalRateLimit(ctx, r)
				if err != nil {
					err = fmt.Errorf("error in checkGlobalRateLimit: %w", err)
					span := trace.SpanFromContext(ctx)
					span.RecordError(err)
					resp.Responses[i] = &RateLimitResponse{Error: err.Error()}
				}

				// Inform the client of the owner key of the key
				resp.Responses[i].Metadata = map[string]string{"owner": peer.Info().HTTPAddress}
				continue
			}

			// Request must be forwarded to peer that owns the key.
			// Launch remote peer request in goroutine.
			wg.Add(1)
			go s.asyncRequest(ctx, &AsyncReq{
				AsyncCh: asyncCh,
				Peer:    peer,
				Req:     r,
				WG:      &wg,
				Key:     key,
				Idx:     i,
			})
		}
	}

	// Wait for any async responses if any
	wg.Wait()

	close(asyncCh)
	for a := range asyncCh {
		resp.Responses[a.Idx] = a.Resp
	}

	return nil
}

type AsyncResp struct {
	Resp *RateLimitResponse
	Idx  int
}

type AsyncReq struct {
	Req     *RateLimitRequest
	WG      *sync.WaitGroup
	AsyncCh chan AsyncResp
	Peer    *Peer
	Key     string
	Idx     int
}

func (s *Service) asyncRequest(ctx context.Context, req *AsyncReq) {
	ctx = tracing.StartScope(ctx, "Service.asyncRequest")
	defer tracing.EndScope(ctx, nil)
	var attempts int
	var err error

	funcTimer := prometheus.NewTimer(metricFuncTimeDuration.WithLabelValues("Service.asyncRequest"))
	defer funcTimer.ObserveDuration()

	reqState := RateLimitContext{IsOwner: req.Peer.Info().IsOwner}
	resp := AsyncResp{
		Idx: req.Idx,
	}

	for {
		if attempts > 5 {
			err = fmt.Errorf("attempts exhausted while communicating with '%s' for '%s': %w",
				req.Peer.Info().HTTPAddress, req.Key, err)
			s.log.LogAttrs(ctx, slog.LevelError, "attempts exhausted while communicating with peer",
				ErrAttr(err),
				slog.String("key", req.Key),
			)
			countError(err, "peer communication failed")
			resp.Resp = &RateLimitResponse{Error: err.Error()}
			break
		}

		// If we are attempting again, the owner of this rate limit might have changed to us!
		if attempts != 0 {
			if reqState.IsOwner {
				resp.Resp, err = s.checkLocalRateLimit(ctx, req.Req, reqState)
				if err != nil {
					err = fmt.Errorf("during checkLocalRateLimit() for '%s': %w", req.Key, err)
					s.log.LogAttrs(ctx, slog.LevelError, "while applying rate limit",
						ErrAttr(err),
						slog.String("key", req.Key),
					)
					resp.Resp = &RateLimitResponse{Error: err.Error()}
				}
				break
			}
		}

		// Make an RPC call to the peer that owns this rate limit
		var r *RateLimitResponse
		r, err = req.Peer.Forward(ctx, req.Req)
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) ||
				errors.Is(err, ErrPeerShutdown) {

				attempts++
				metricBatchSendRetries.WithLabelValues(req.Req.Name).Inc()
				req.Peer, err = s.GetPeer(ctx, req.Key)
				if err != nil {
					err = fmt.Errorf("while finding peer that owns rate limit '%s': %w", req.Key, err)
					s.log.LogAttrs(ctx, slog.LevelError, err.Error(),
						ErrAttr(err),
						slog.String("key", req.Key),
					)
					countError(err, "during GetPeer()")
					resp.Resp = &RateLimitResponse{Error: err.Error()}
					break
				}
				continue
			}

			// Not calling `countError()` because we expect the remote end to report this error.
			err = fmt.Errorf("while fetching rate limit '%s' from peer: %w", req.Key, err)
			resp.Resp = &RateLimitResponse{Error: err.Error()}
			break
		}

		// Inform the client of the owner key of the key
		resp.Resp = r
		resp.Resp.Metadata = map[string]string{"owner": req.Peer.Info().HTTPAddress}
		break
	}

	req.AsyncCh <- resp
	req.WG.Done()

	if isDeadlineExceeded(ctx.Err()) {
		metricCheckErrorCounter.WithLabelValues("Timeout forwarding to peer").Inc()
	}
}

// checkGlobalRateLimit handles rate limits that are marked as `Behavior = GLOBAL`. Rate limit responses
// are returned from the local cache and the hits are queued to be sent to the owning peer.
func (s *Service) checkGlobalRateLimit(ctx context.Context, req *RateLimitRequest) (resp *RateLimitResponse, err error) {
	ctx = tracing.StartScope(ctx, "Service.checkGlobalRateLimit", trace.WithAttributes(
		attribute.String("ratelimit.key", req.UniqueKey),
		attribute.String("ratelimit.name", req.Name),
	))
	defer prometheus.NewTimer(metricFuncTimeDuration.WithLabelValues("Service.checkGlobalRateLimit")).ObserveDuration()
	defer func() {
		if err == nil {
			s.global.QueueHit(req)
		}
		tracing.EndScope(ctx, err)
	}()

	req2 := proto.Clone(req).(*RateLimitRequest)
	SetBehavior(&req2.Behavior, Behavior_NO_BATCHING, true)
	SetBehavior(&req2.Behavior, Behavior_GLOBAL, false)
	reqState := RateLimitContext{IsOwner: false}

	// Process the rate limit like we own it
	resp, err = s.checkLocalRateLimit(ctx, req2, reqState)
	if err != nil {
		return nil, fmt.Errorf("during in checkLocalRateLimit: %w", err)
	}

	metricGetRateLimitCounter.WithLabelValues("global").Inc()
	return resp, nil
}

// Update updates the local cache with a list of rate limit state from a peer
// This method should only be called by a peer.
func (s *Service) Update(ctx context.Context, r *UpdateRequest, _ *v1.Reply) (err error) {
	ctx = tracing.StartScope(ctx, "Service.Update")
	defer func() { tracing.EndScope(ctx, err) }()

	now := MillisecondNow()

	for _, g := range r.Globals {
		item, _, err := s.cache.GetCacheItem(ctx, g.Key)
		if err != nil {
			return err
		}

		if item == nil {
			item = &CacheItem{
				ExpireAt:  g.State.ResetTime,
				Algorithm: g.Algorithm,
				Key:       g.Key,
			}
			err := s.cache.AddCacheItem(ctx, g.Key, item)
			if err != nil {
				return fmt.Errorf("during CacheManager.AddCacheItem(): %w", err)
			}
		}

		item.mutex.Lock()
		switch g.Algorithm {
		case Algorithm_LEAKY_BUCKET:
			item.Value = &LeakyBucketItem{
				Remaining: float64(g.State.Remaining),
				Limit:     g.State.Limit,
				Duration:  g.Duration,
				Burst:     g.State.Limit,
				UpdatedAt: now,
			}
		case Algorithm_TOKEN_BUCKET:
			item.Value = &TokenBucketItem{
				Status:    g.State.Status,
				Limit:     g.State.Limit,
				Duration:  g.Duration,
				Remaining: g.State.Remaining,
				CreatedAt: now,
			}
		}
		item.mutex.Unlock()
	}
	return nil
}

// Forward is called by other peers when forwarding rate limits to this peer
func (s *Service) Forward(ctx context.Context, req *ForwardRequest, resp *ForwardResponse) (err error) {
	if len(req.Requests) > maxBatchSize {
		metricCheckErrorCounter.WithLabelValues("Request too large").Add(1)
		return duh.NewServiceError(duh.CodeBadRequest,
			fmt.Sprintf("'Forward.requests' list too large; max size is '%d'", maxBatchSize), nil, nil)
	}

	// Invoke each rate limit request.
	type respOut struct {
		idx int
		rl  *RateLimitResponse
	}

	resp.RateLimits = make([]*RateLimitResponse, len(req.Requests))
	reqState := RateLimitContext{IsOwner: true}
	respChan := make(chan respOut)
	var respWg sync.WaitGroup
	respWg.Add(1)

	go func() {
		// Capture each response and return in the same order
		for out := range respChan {
			resp.RateLimits[out.idx] = out.rl
		}

		respWg.Done()
	}()

	// Fan out requests.
	fan := wait.NewFanOut(s.conf.Workers)
	for idx, req := range req.Requests {
		fan.Run(func() error {
			// Extract the propagated context from the metadata in the request
			ctx := s.propagator.Extract(ctx, &MetadataCarrier{Map: req.Metadata})

			// Forwarded global requests must have DRAIN_OVER_LIMIT set so token and leaky algorithms
			// drain the remaining in the event a peer asks for more than is remaining.
			// This is needed because with GLOBAL behavior peers will accumulate hits, which could
			// result in requesting more hits than is remaining.
			if HasBehavior(req.Behavior, Behavior_GLOBAL) {
				SetBehavior(&req.Behavior, Behavior_DRAIN_OVER_LIMIT, true)
			}

			// Assign default to CreatedAt for backwards compatibility.
			if req.CreatedAt == nil || *req.CreatedAt == 0 {
				createdAt := epochMillis(clock.Now())
				req.CreatedAt = &createdAt
			}

			rl, err := s.checkLocalRateLimit(ctx, req, reqState)
			if err != nil {
				rl = &RateLimitResponse{Error: fmt.Errorf("error in checkLocalRateLimit: %w", err).Error()}
				// metricCheckErrorCounter is updated within checkLocalRateLimit(), not in Forward().
			}

			respChan <- respOut{idx, rl}
			return nil
		})
	}

	// Wait for all requests to be handled, then clean up.
	_ = fan.Wait()
	close(respChan)
	respWg.Wait()

	return nil
}

// HealthCheck Returns the health of our instance.
func (s *Service) HealthCheck(ctx context.Context, _ *HealthCheckRequest, resp *HealthCheckResponse) (err error) {

	var errs []string

	s.peerMutex.RLock()
	defer s.peerMutex.RUnlock()

	// Iterate through local peers and get their last errors
	localPeers := s.conf.LocalPicker.Peers()
	for _, peer := range localPeers {
		for _, errMsg := range peer.GetLastErr() {
			err := fmt.Errorf("error returned from local peer.GetLastErr: %s", errMsg)
			errs = append(errs, err.Error())
		}
	}

	// Do the same for region peers
	regionPeers := s.conf.RegionPicker.Peers()
	for _, peer := range regionPeers {
		for _, errMsg := range peer.GetLastErr() {
			err := fmt.Errorf("error returned from region peer.GetLastErr: %s", errMsg)
			errs = append(errs, err.Error())
		}
	}

	resp.PeerCount = int32(len(localPeers) + len(regionPeers))
	resp.Status = Healthy

	if len(errs) != 0 {
		resp.Status = UnHealthy
		resp.Message = strings.Join(errs, "|")
	}

	return nil
}

func (s *Service) checkLocalRateLimit(ctx context.Context, r *RateLimitRequest, reqState RateLimitContext) (_ *RateLimitResponse, err error) {
	ctx = tracing.StartScope(ctx, "Service.checkLocalRateLimit", trace.WithAttributes(
		attribute.String("ratelimit.key", r.UniqueKey),
		attribute.String("ratelimit.name", r.Name),
		attribute.Int64("ratelimit.limit", r.Limit),
		attribute.Int64("ratelimit.hits", r.Hits),
	))
	defer func() { tracing.EndScope(ctx, err) }()
	defer prometheus.NewTimer(metricFuncTimeDuration.WithLabelValues("Service.checkLocalRateLimit")).ObserveDuration()

	resp, err := s.cache.CheckRateLimit(ctx, r, reqState)
	if err != nil {
		return nil, fmt.Errorf("during CacheManager.CheckRateLimit: %w", err)
	}

	// If global behavior, then broadcast update to all peers.
	if HasBehavior(r.Behavior, Behavior_GLOBAL) {
		s.global.QueueUpdate(r)
	}

	if reqState.IsOwner {
		metricGetRateLimitCounter.WithLabelValues("local").Inc()

		// Send to event channel, if set.
		if s.conf.EventChannel != nil {
			e := HitEvent{
				Request:  r,
				Response: resp,
			}
			select {
			case s.conf.EventChannel <- e:
			case <-ctx.Done():
			}
		}
	}
	return resp, nil
}

// SetPeers replaces the peers and shuts down all the previous peers.
func (s *Service) SetPeers(peerInfo []PeerInfo) {
	localPicker := s.conf.LocalPicker.New()
	regionPicker := s.conf.RegionPicker.New()

	for _, info := range peerInfo {
		// Add peers that are not in our local DC to the RegionPicker
		if info.DataCenter != s.conf.DataCenter {
			peer := s.conf.RegionPicker.GetByPeerInfo(info)
			// If we don't have an existing Peer create a new one
			if peer == nil {
				var err error
				peer, err = NewPeer(PeerConfig{
					PeerClient: s.conf.PeerClientFactory(info),
					Behavior:   s.conf.Behaviors,
					Log:        s.log,
					Info:       info,
				})
				if err != nil {
					s.log.LogAttrs(context.TODO(), slog.LevelError, "during NewPeer() call",
						ErrAttr(err),
						slog.String("http_address", info.HTTPAddress),
					)
					return
				}
			}
			regionPicker.Add(peer)
			continue
		}
		// If we don't have an existing Peer create a new one
		peer := s.conf.LocalPicker.GetByPeerInfo(info)
		if peer == nil {
			var err error
			peer, err = NewPeer(PeerConfig{
				PeerClient: s.conf.PeerClientFactory(info),
				Behavior:   s.conf.Behaviors,
				Log:        s.log,
				Info:       info,
			})
			if err != nil {
				s.log.LogAttrs(context.TODO(), slog.LevelError, "during NewPeer() call",
					ErrAttr(err),
					slog.String("http_address", info.HTTPAddress),
				)
				return
			}
		}
		localPicker.Add(peer)
	}

	s.peerMutex.Lock()

	// Replace our current pickers
	oldLocalPicker := s.conf.LocalPicker
	oldRegionPicker := s.conf.RegionPicker
	s.conf.LocalPicker = localPicker
	s.conf.RegionPicker = regionPicker
	s.peerMutex.Unlock()

	s.log.LogAttrs(context.TODO(), slog.LevelDebug, "peers updated",
		slog.Any("peers", peerInfo),
	)

	// Shutdown any old peers we no longer need
	ctx, cancel := context.WithTimeout(context.Background(), s.conf.Behaviors.BatchTimeout)
	defer cancel()

	var shutdownPeers []*Peer
	for _, peer := range oldLocalPicker.Peers() {
		if peerInfo := s.conf.LocalPicker.GetByPeerInfo(peer.Info()); peerInfo == nil {
			shutdownPeers = append(shutdownPeers, peer)
		}
	}

	for _, regionPicker := range oldRegionPicker.Pickers() {
		for _, peer := range regionPicker.Peers() {
			if peerInfo := s.conf.RegionPicker.GetByPeerInfo(peer.Info()); peerInfo == nil {
				shutdownPeers = append(shutdownPeers, peer)
			}
		}
	}

	var wg wait.Group
	for _, p := range shutdownPeers {
		wg.Run(func() error {
			err := p.Close(ctx)
			if err != nil {
				s.log.LogAttrs(context.TODO(), slog.LevelError, "while shutting down peer",
					ErrAttr(err),
					slog.Any("peer", p),
				)
			}
			return nil
		})
	}
	_ = wg.Wait()

	if len(shutdownPeers) > 0 {
		var peers []string
		for _, p := range shutdownPeers {
			peers = append(peers, p.Info().HTTPAddress)
		}
		s.log.LogAttrs(context.TODO(), slog.LevelDebug, "peers shutdown",
			slog.Any("peers", peers),
		)
	}
}

// GetPeer returns a peer client for the hash key provided
func (s *Service) GetPeer(_ context.Context, key string) (p *Peer, err error) {
	s.peerMutex.RLock()
	defer s.peerMutex.RUnlock()

	p, err = s.conf.LocalPicker.Get(key)
	if err != nil {
		return nil, fmt.Errorf("error in conf.LocalPicker.Get: %w", err)
	}

	return p, nil
}

func (s *Service) GetPeerList() []*Peer {
	s.peerMutex.RLock()
	defer s.peerMutex.RUnlock()
	return s.conf.LocalPicker.Peers()
}

func (s *Service) GetRegionPickers() map[string]PeerPicker {
	s.peerMutex.RLock()
	defer s.peerMutex.RUnlock()
	return s.conf.RegionPicker.Pickers()
}

// Describe fetches prometheus metrics to be registered
func (s *Service) Describe(ch chan<- *prometheus.Desc) {
	metricBatchQueueLength.Describe(ch)
	metricBatchSendDuration.Describe(ch)
	metricBatchSendRetries.Describe(ch)
	metricCheckErrorCounter.Describe(ch)
	metricConcurrentChecks.Describe(ch)
	metricFuncTimeDuration.Describe(ch)
	metricGetRateLimitCounter.Describe(ch)
	metricOverLimitCounter.Describe(ch)
	s.global.metricBroadcastDuration.Describe(ch)
	s.global.metricGlobalQueueLength.Describe(ch)
	s.global.metricGlobalSendDuration.Describe(ch)
	s.global.metricGlobalSendQueueLength.Describe(ch)
}

// Collect fetches metrics from the server for use by prometheus
func (s *Service) Collect(ch chan<- prometheus.Metric) {
	metricBatchQueueLength.Collect(ch)
	metricBatchSendDuration.Collect(ch)
	metricBatchSendRetries.Collect(ch)
	metricCheckErrorCounter.Collect(ch)
	metricConcurrentChecks.Collect(ch)
	metricFuncTimeDuration.Collect(ch)
	metricGetRateLimitCounter.Collect(ch)
	metricOverLimitCounter.Collect(ch)
	s.global.metricBroadcastDuration.Collect(ch)
	s.global.metricGlobalQueueLength.Collect(ch)
	s.global.metricGlobalSendDuration.Collect(ch)
	s.global.metricGlobalSendQueueLength.Collect(ch)
}

// HasBehavior returns true if the provided behavior is set
func HasBehavior(b Behavior, flag Behavior) bool {
	return b&flag != 0
}

// SetBehavior sets or clears the behavior depending on the boolean `set`
func SetBehavior(b *Behavior, flag Behavior, set bool) {
	if set {
		*b = *b | flag
	} else {
		mask := *b ^ flag
		*b &= mask
	}
}

// Count an error type in the metricCheckErrorCounter metric.
// Recurse into wrapped errors if necessary.
func countError(err error, defaultType string) {
	for {
		if err == nil {
			metricCheckErrorCounter.WithLabelValues(defaultType).Inc()
			return
		}
		if errors.Is(err, context.DeadlineExceeded) {
			metricCheckErrorCounter.WithLabelValues("Timeout").Inc()
			return
		}

		err = errors.Unwrap(err)
	}
}

func isDeadlineExceeded(err error) bool {
	if err == nil {
		return false
	}
	return errors.Is(err, context.DeadlineExceeded)
}

func epochMillis(t time.Time) int64 {
	return t.UnixNano() / 1_000_000
}

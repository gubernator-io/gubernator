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
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mailgun/errors"
	"github.com/mailgun/holster/v4/clock"
	"github.com/mailgun/holster/v4/syncutil"
	"github.com/mailgun/holster/v4/tracing"
	"github.com/prometheus/client_golang/prometheus"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
	spb "google.golang.org/genproto/googleapis/rpc/status"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

const (
	maxBatchSize = 1000
	Healthy      = "healthy"
	UnHealthy    = "unhealthy"
)

type V1Instance struct {
	UnimplementedV1Server
	UnimplementedPeersV1Server
	global     *globalManager
	peerMutex  sync.RWMutex
	log        FieldLogger
	conf       Config
	isClosed   atomic.Bool
	workerPool *WorkerPool
}

type RateLimitReqState struct {
	IsOwner bool
}

var (
	metricGetRateLimitCounter = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "gubernator_getratelimit_counter",
		Help: "The count of getLocalRateLimit() calls.  Label \"calltype\" may be \"local\" for calls handled by the same peer, or \"global\" for global rate limits.",
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
	metricCommandCounter = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "gubernator_command_counter",
		Help: "The count of commands processed by each worker in WorkerPool.",
	}, []string{"worker", "method"})
	metricWorkerQueue = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "gubernator_worker_queue_length",
		Help: "The count of requests queued up in WorkerPool.",
	}, []string{"method", "worker"})

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
	metricUpdatePeerGlobalsCounter = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "gubernator_updatepeerglobals_counter",
		Help: "The count of items received in UpdatePeerGlobals",
	})
)

// NewV1Instance instantiate a single instance of a gubernator peer and register this
// instance with the provided GRPCServer.
func NewV1Instance(conf Config) (s *V1Instance, err error) {
	ctx := context.Background()
	if conf.GRPCServers == nil {
		return nil, errors.New("at least one GRPCServer instance is required")
	}
	if err := conf.SetDefaults(); err != nil {
		return nil, err
	}

	s = &V1Instance{
		log:  conf.Logger,
		conf: conf,
	}

	s.workerPool = NewWorkerPool(&conf)
	s.global = newGlobalManager(conf.Behaviors, s)

	// Register our instance with all GRPC servers
	for _, srv := range conf.GRPCServers {
		RegisterV1Server(srv, s)
		RegisterPeersV1Server(srv, s)
	}

	if s.conf.Loader == nil {
		return s, nil
	}

	// Load the cache.
	err = s.workerPool.Load(ctx)
	if err != nil {
		return nil, errors.Wrap(err, "Error in workerPool.Load")
	}

	return s, nil
}

func (s *V1Instance) Close() (err error) {
	if !s.isClosed.CompareAndSwap(false, true) {
		return nil
	}

	s.global.Close()

	if s.conf.Loader != nil {
		err = s.workerPool.Store(context.Background())
		if err != nil {
			s.log.WithError(err).
				Error("Error in workerPool.Store")
			return errors.Wrap(err, "Error in workerPool.Store")
		}
	}

	err = s.workerPool.Close()
	if err != nil {
		s.log.WithError(err).
			Error("Error in workerPool.Close")
		return errors.Wrap(err, "Error in workerPool.Close")
	}

	return nil
}

// GetRateLimits is the public interface used by clients to request rate limits from the system. If the
// rate limit `Name` and `UniqueKey` is not owned by this instance, then we forward the request to the
// peer that does.
func (s *V1Instance) GetRateLimits(ctx context.Context, r *GetRateLimitsReq) (_ *GetRateLimitsResp, err error) {
	span := trace.SpanFromContext(ctx)
	span.SetAttributes(
		attribute.Int("item.count", len(r.Requests)),
	)
	funcTimer := prometheus.NewTimer(metricFuncTimeDuration.WithLabelValues("V1Instance.GetRateLimits"))
	defer funcTimer.ObserveDuration()
	metricConcurrentChecks.Inc()
	defer metricConcurrentChecks.Dec()

	if len(r.Requests) > maxBatchSize {
		metricCheckErrorCounter.WithLabelValues("Request too large").Inc()
		return nil, status.Errorf(codes.OutOfRange,
			"Requests.RateLimits list too large; max size is '%d'", maxBatchSize)
	}

	createdAt := epochMillis(clock.Now())
	resp := GetRateLimitsResp{
		Responses: make([]*RateLimitResp, len(r.Requests)),
	}
	var wg sync.WaitGroup
	asyncCh := make(chan AsyncResp, len(r.Requests))

	// For each item in the request body
	for i, req := range r.Requests {
		key := req.Name + "_" + req.UniqueKey
		var peer *PeerClient
		var err error

		if req.UniqueKey == "" {
			metricCheckErrorCounter.WithLabelValues("Invalid request").Inc()
			resp.Responses[i] = &RateLimitResp{Error: "field 'unique_key' cannot be empty"}
			continue
		}
		if req.Name == "" {
			metricCheckErrorCounter.WithLabelValues("Invalid request").Inc()
			resp.Responses[i] = &RateLimitResp{Error: "field 'namespace' cannot be empty"}
			continue
		}
		if req.CreatedAt == nil || *req.CreatedAt == 0 {
			req.CreatedAt = &createdAt
		}

		if ctx.Err() != nil {
			err = errors.Wrap(ctx.Err(), "Error while iterating request items")
			span := trace.SpanFromContext(ctx)
			span.RecordError(err)
			resp.Responses[i] = &RateLimitResp{
				Error: err.Error(),
			}
			continue
		}

		if s.conf.Behaviors.ForceGlobal {
			SetBehavior(&req.Behavior, Behavior_GLOBAL, true)
		}

		peer, err = s.GetPeer(ctx, key)
		if err != nil {
			countError(err, "Error in GetPeer")
			err = errors.Wrapf(err, "Error in GetPeer, looking up peer that owns rate limit '%s'", key)
			resp.Responses[i] = &RateLimitResp{
				Error: err.Error(),
			}
			continue
		}

		// If our server instance is the owner of this rate limit
		reqState := RateLimitReqState{IsOwner: peer.Info().IsOwner}
		if reqState.IsOwner {
			// Apply our rate limit algorithm to the request
			resp.Responses[i], err = s.getLocalRateLimit(ctx, req, reqState)
			if err != nil {
				err = errors.Wrapf(err, "Error while apply rate limit for '%s'", key)
				span := trace.SpanFromContext(ctx)
				span.RecordError(err)
				resp.Responses[i] = &RateLimitResp{Error: err.Error()}
			}
		} else {
			if HasBehavior(req.Behavior, Behavior_GLOBAL) {
				resp.Responses[i], err = s.getGlobalRateLimit(ctx, req)
				if err != nil {
					err = errors.Wrap(err, "Error in getGlobalRateLimit")
					span := trace.SpanFromContext(ctx)
					span.RecordError(err)
					resp.Responses[i] = &RateLimitResp{Error: err.Error()}
				}

				// Inform the client of the owner key of the key
				resp.Responses[i].Metadata = map[string]string{"owner": peer.Info().GRPCAddress}
				continue
			}

			// Request must be forwarded to peer that owns the key.
			// Launch remote peer request in goroutine.
			wg.Add(1)
			go s.asyncRequest(ctx, &AsyncReq{
				AsyncCh: asyncCh,
				Peer:    peer,
				Req:     req,
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

	return &resp, nil
}

type AsyncResp struct {
	Idx  int
	Resp *RateLimitResp
}

type AsyncReq struct {
	WG      *sync.WaitGroup
	AsyncCh chan AsyncResp
	Req     *RateLimitReq
	Peer    *PeerClient
	Key     string
	Idx     int
}

func (s *V1Instance) asyncRequest(ctx context.Context, req *AsyncReq) {
	var attempts int
	var err error

	ctx = tracing.StartNamedScope(ctx, "V1Instance.asyncRequest")
	defer tracing.EndScope(ctx, nil)

	funcTimer := prometheus.NewTimer(metricFuncTimeDuration.WithLabelValues("V1Instance.asyncRequest"))
	defer funcTimer.ObserveDuration()

	reqState := RateLimitReqState{IsOwner: req.Peer.Info().IsOwner}
	resp := AsyncResp{
		Idx: req.Idx,
	}

	for {
		if attempts > 5 {
			s.log.WithContext(ctx).
				WithError(err).
				WithField("key", req.Key).
				Error("GetPeer() returned peer that is not connected")
			countError(err, "Peer not connected")
			err = fmt.Errorf("GetPeer() keeps returning peers that are not connected for '%s': %w", req.Key, err)
			resp.Resp = &RateLimitResp{Error: err.Error()}
			break
		}

		// If we are attempting again, the owner of this rate limit might have changed to us!
		if attempts != 0 {
			if reqState.IsOwner {
				resp.Resp, err = s.getLocalRateLimit(ctx, req.Req, reqState)
				if err != nil {
					s.log.WithContext(ctx).
						WithError(err).
						WithField("key", req.Key).
						Error("Error applying rate limit")
					err = fmt.Errorf("during getLocalRateLimit() for '%s': %w", req.Key, err)
					resp.Resp = &RateLimitResp{Error: err.Error()}
				}
				break
			}
		}

		// Make an RPC call to the peer that owns this rate limit
		var r *RateLimitResp
		r, err = req.Peer.GetPeerRateLimit(ctx, req.Req)
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				attempts++
				metricBatchSendRetries.WithLabelValues(req.Req.Name).Inc()
				req.Peer, err = s.GetPeer(ctx, req.Key)
				if err != nil {
					errPart := fmt.Sprintf("while finding peer that owns rate limit '%s'", req.Key)
					s.log.WithContext(ctx).WithError(err).WithField("key", req.Key).Error(errPart)
					countError(err, "during GetPeer()")
					err = fmt.Errorf("%s: %w", errPart, err)
					resp.Resp = &RateLimitResp{Error: err.Error()}
					break
				}
				continue
			}

			// Not calling `countError()` because we expect the remote end to
			// report this error.
			err = fmt.Errorf("while fetching rate limit '%s' from peer: %w", req.Key, err)
			resp.Resp = &RateLimitResp{Error: err.Error()}
			break
		}

		// Inform the client of the owner key of the key
		resp.Resp = r
		resp.Resp.Metadata = map[string]string{"owner": req.Peer.Info().GRPCAddress}
		break
	}

	req.AsyncCh <- resp
	req.WG.Done()

	if isDeadlineExceeded(ctx.Err()) {
		metricCheckErrorCounter.WithLabelValues("Timeout forwarding to peer").Inc()
	}
}

// getGlobalRateLimit handles rate limits that are marked as `Behavior = GLOBAL`. Rate limit responses
// are returned from the local cache and the hits are queued to be sent to the owning peer.
func (s *V1Instance) getGlobalRateLimit(ctx context.Context, req *RateLimitReq) (resp *RateLimitResp, err error) {
	ctx = tracing.StartNamedScope(ctx, "V1Instance.getGlobalRateLimit", trace.WithAttributes(
		attribute.String("ratelimit.key", req.UniqueKey),
		attribute.String("ratelimit.name", req.Name),
	))
	defer prometheus.NewTimer(metricFuncTimeDuration.WithLabelValues("V1Instance.getGlobalRateLimit")).ObserveDuration()
	defer func() {
		if err == nil {
			s.global.QueueHit(req)
		}
		tracing.EndScope(ctx, err)
	}()

	req2 := proto.Clone(req).(*RateLimitReq)
	SetBehavior(&req2.Behavior, Behavior_NO_BATCHING, true)
	SetBehavior(&req2.Behavior, Behavior_GLOBAL, false)
	reqState := RateLimitReqState{IsOwner: false}

	// Process the rate limit like we own it
	resp, err = s.getLocalRateLimit(ctx, req2, reqState)
	if err != nil {
		return nil, errors.Wrap(err, "during in getLocalRateLimit")
	}

	metricGetRateLimitCounter.WithLabelValues("global").Inc()
	return resp, nil
}

// UpdatePeerGlobals received global broadcasts.  This updates the local cache with a list of
// global rate limits. This method should only be called by a peer who is the owner of a global
// rate limit.
func (s *V1Instance) UpdatePeerGlobals(ctx context.Context, r *UpdatePeerGlobalsReq) (_ *UpdatePeerGlobalsResp, err error) {
	span := trace.SpanFromContext(ctx)
	span.SetAttributes(
		attribute.Int("item.count", len(r.Globals)),
	)
	defer func() { tracing.EndScope(ctx, err) }()
	defer prometheus.NewTimer(metricFuncTimeDuration.WithLabelValues("V1Instance.UpdatePeerGlobals")).ObserveDuration()
	metricUpdatePeerGlobalsCounter.Add(float64(len(r.Globals)))
	now := MillisecondNow()
	for _, g := range r.Globals {
		item := &CacheItem{
			ExpireAt:  g.Status.ResetTime,
			Algorithm: g.Algorithm,
			Key:       g.Key,
		}
		switch g.Algorithm {
		case Algorithm_LEAKY_BUCKET:
			item.Value = &LeakyBucketItem{
				Remaining: float64(g.Status.Remaining),
				Limit:     g.Status.Limit,
				Duration:  g.Duration,
				Burst:     g.Status.Limit,
				UpdatedAt: now,
			}
		case Algorithm_TOKEN_BUCKET:
			item.Value = &TokenBucketItem{
				Status:    g.Status.Status,
				Limit:     g.Status.Limit,
				Duration:  g.Duration,
				Remaining: g.Status.Remaining,
				CreatedAt: now,
			}
		}
		err = s.workerPool.AddCacheItem(ctx, g.Key, item)
		if err != nil {
			return nil, errors.Wrap(err, "Error in workerPool.AddCacheItem")
		}
	}

	return &UpdatePeerGlobalsResp{}, nil
}

// GetPeerRateLimits is called by other peers to get the rate limits owned by this peer.
func (s *V1Instance) GetPeerRateLimits(ctx context.Context, r *GetPeerRateLimitsReq) (resp *GetPeerRateLimitsResp, err error) {
	span := trace.SpanFromContext(ctx)
	span.SetAttributes(
		attribute.Int("item.count", len(r.Requests)),
	)
	defer func() { tracing.EndScope(ctx, err) }()
	defer prometheus.NewTimer(metricFuncTimeDuration.WithLabelValues("V1Instance.GetPeerRateLimits")).ObserveDuration()
	if len(r.Requests) > maxBatchSize {
		err := fmt.Errorf("'PeerRequest.rate_limits' list too large; max size is '%d'", maxBatchSize)
		metricCheckErrorCounter.WithLabelValues("Request too large").Inc()
		return nil, status.Error(codes.OutOfRange, err.Error())
	}

	// Invoke each rate limit request.
	type reqIn struct {
		idx int
		req *RateLimitReq
	}
	type respOut struct {
		idx int
		rl  *RateLimitResp
	}

	resp = &GetPeerRateLimitsResp{
		RateLimits: make([]*RateLimitResp, len(r.Requests)),
	}
	respChan := make(chan respOut)
	var respWg sync.WaitGroup
	respWg.Add(1)
	reqState := RateLimitReqState{IsOwner: true}

	go func() {
		// Capture each response and return in the same order
		for out := range respChan {
			resp.RateLimits[out.idx] = out.rl
		}

		respWg.Done()
	}()

	// Fan out requests.
	fan := syncutil.NewFanOut(s.conf.Workers)
	for idx, req := range r.Requests {
		fan.Run(func(in any) error {
			rin := in.(reqIn)
			// Extract the propagated context from the metadata in the request
			prop := propagation.TraceContext{}
			ctx := prop.Extract(ctx, &MetadataCarrier{Map: rin.req.Metadata})

			// Forwarded global requests must have DRAIN_OVER_LIMIT set so token and leaky algorithms
			// drain the remaining in the event a peer asks for more than is remaining.
			// This is needed because with GLOBAL behavior peers will accumulate hits, which could
			// result in requesting more hits than is remaining.
			if HasBehavior(rin.req.Behavior, Behavior_GLOBAL) {
				SetBehavior(&rin.req.Behavior, Behavior_DRAIN_OVER_LIMIT, true)
			}

			// Assign default to CreatedAt for backwards compatibility.
			if rin.req.CreatedAt == nil || *rin.req.CreatedAt == 0 {
				createdAt := epochMillis(clock.Now())
				rin.req.CreatedAt = &createdAt
			}

			rl, err := s.getLocalRateLimit(ctx, rin.req, reqState)
			if err != nil {
				// Return the error for this request
				err = errors.Wrap(err, "Error in getLocalRateLimit")
				rl = &RateLimitResp{Error: err.Error()}
				// metricCheckErrorCounter is updated within getLocalRateLimit(), not in GetPeerRateLimits.
			}

			respChan <- respOut{rin.idx, rl}
			return nil
		}, reqIn{idx, req})
	}

	// Wait for all requests to be handled, then clean up.
	_ = fan.Wait()
	close(respChan)
	respWg.Wait()

	return resp, nil
}

// HealthCheck Returns the health of our instance.
func (s *V1Instance) HealthCheck(ctx context.Context, r *HealthCheckReq) (health *HealthCheckResp, err error) {
	span := trace.SpanFromContext(ctx)

	var errs []string
	ownPeerAddress := ""

	s.peerMutex.RLock()
	defer s.peerMutex.RUnlock()

	// Iterate through local peers and get their last errors
	localPeers := s.conf.LocalPicker.Peers()
	for _, peer := range localPeers {
		for _, errMsg := range peer.GetLastErr() {
			err := fmt.Errorf("error returned from local peer.GetLastErr: %s", errMsg)
			span.RecordError(err)
			errs = append(errs, err.Error())
		}

		if ownPeerAddress == "" && peer.Info().GRPCAddress == s.conf.AdvertiseAddr {
			ownPeerAddress = peer.Info().GRPCAddress
		}
	}

	// Do the same for region peers
	regionPeers := s.conf.RegionPicker.Peers()
	for _, peer := range regionPeers {
		for _, errMsg := range peer.GetLastErr() {
			err := fmt.Errorf("error returned from region peer.GetLastErr: %s", errMsg)
			span.RecordError(err)
			errs = append(errs, err.Error())
		}

		if ownPeerAddress == "" && peer.Info().GRPCAddress == s.conf.AdvertiseAddr &&
			peer.Info().DataCenter == s.conf.DataCenter {
			ownPeerAddress = peer.Info().GRPCAddress
		}
	}

	health = &HealthCheckResp{
		PeerCount:        int32(len(localPeers) + len(regionPeers)),
		Status:           Healthy,
		AdvertiseAddress: ownPeerAddress,
	}

	if len(errs) != 0 {
		health.Status = UnHealthy
		health.Message = strings.Join(errs, "|")
	}

	if health.AdvertiseAddress == "" {
		health.Status = UnHealthy
		health.Message = strings.Join(append(errs, "this instance is not found in the peer list"), "|")
	}

	span.SetAttributes(
		attribute.Int64("health.peerCount", int64(health.PeerCount)),
		attribute.String("health.status", health.Status),
	)

	s.log.WithFields(map[string]any{
		"conf.advertiseAddress": s.conf.AdvertiseAddr,
		"health.peerCount":      int64(health.PeerCount),
		"health.status":         health.Status,
	}).Debug("health check")

	if health.Status != Healthy {
		return nil, status.ErrorProto(&spb.Status{
			Code:    int32(codes.Unavailable),
			Message: health.Message,
		})
	}

	return health, nil
}

// LiveCheck simply allows checking if the server is running.
func (s *V1Instance) LiveCheck(_ context.Context, _ *LiveCheckReq) (health *LiveCheckResp, err error) {
	if s.isClosed.Load() {
		return nil, status.Error(codes.Unavailable, "server is shutting down")
	}
	return &LiveCheckResp{}, nil
}

func (s *V1Instance) getLocalRateLimit(ctx context.Context, r *RateLimitReq, reqState RateLimitReqState) (_ *RateLimitResp, err error) {
	ctx = tracing.StartNamedScope(ctx, "V1Instance.getLocalRateLimit", trace.WithAttributes(
		attribute.String("ratelimit.key", r.UniqueKey),
		attribute.String("ratelimit.name", r.Name),
		attribute.Int64("ratelimit.limit", r.Limit),
		attribute.Int64("ratelimit.hits", r.Hits),
		attribute.Int64("ratelimit.duration", r.Duration),
	))
	defer func() { tracing.EndScope(ctx, err) }()
	defer prometheus.NewTimer(metricFuncTimeDuration.WithLabelValues("V1Instance.getLocalRateLimit")).ObserveDuration()

	resp, err := s.workerPool.GetRateLimit(ctx, r, reqState)
	if err != nil {
		return nil, errors.Wrap(err, "during workerPool.GetRateLimit")
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
// TODO this should return an error if we failed to connect to any of the new peers
func (s *V1Instance) SetPeers(peerInfo []PeerInfo) {
	localPicker := s.conf.LocalPicker.New()
	regionPicker := s.conf.RegionPicker.New()

	for _, info := range peerInfo {
		// Add peers that are not in our local DC to the RegionPicker
		if info.DataCenter != s.conf.DataCenter {
			peer := s.conf.RegionPicker.GetByPeerInfo(info)
			// If we don't have an existing PeerClient create a new one
			if peer == nil {
				var err error
				peer, err = NewPeerClient(PeerConfig{
					TraceGRPC: s.conf.PeerTraceGRPC,
					Behavior:  s.conf.Behaviors,
					TLS:       s.conf.PeerTLS,
					Log:       s.log,
					Info:      info,
				})
				if err != nil {
					s.log.Errorf("error connecting to peer %s: %s", info.GRPCAddress, err)
					return
				}
			}
			regionPicker.Add(peer)
			continue
		}
		// If we don't have an existing PeerClient create a new one
		peer := s.conf.LocalPicker.GetByPeerInfo(info)
		if peer == nil {
			var err error
			peer, err = NewPeerClient(PeerConfig{
				TraceGRPC: s.conf.PeerTraceGRPC,
				Behavior:  s.conf.Behaviors,
				TLS:       s.conf.PeerTLS,
				Log:       s.log,
				Info:      info,
			})
			if err != nil {
				s.log.Errorf("error connecting to peer %s: %s", info.GRPCAddress, err)
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

	s.log.WithField("peers", peerInfo).Debug("peers updated")

	// Shutdown any old peers we no longer need
	ctx, cancel := context.WithTimeout(context.Background(), s.conf.Behaviors.BatchTimeout)
	defer cancel()

	var shutdownPeers []*PeerClient
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

	var wg syncutil.WaitGroup
	for _, p := range shutdownPeers {
		wg.Run(func(obj any) error {
			pc := obj.(*PeerClient)
			err := pc.Shutdown(ctx)
			if err != nil {
				s.log.WithError(err).WithField("peer", pc).Error("while shutting down peer")
			}
			return nil
		}, p)
	}
	wg.Wait()

	if len(shutdownPeers) > 0 {
		var peers []string
		for _, p := range shutdownPeers {
			peers = append(peers, p.Info().GRPCAddress)
		}
		s.log.WithField("peers", peers).Debug("peers shutdown")
	}
}

// GetPeer returns a peer client for the hash key provided
func (s *V1Instance) GetPeer(ctx context.Context, key string) (p *PeerClient, err error) {
	defer prometheus.NewTimer(metricFuncTimeDuration.WithLabelValues("V1Instance.GetPeer")).ObserveDuration()

	s.peerMutex.RLock()
	defer s.peerMutex.RUnlock()
	p, err = s.conf.LocalPicker.Get(key)
	if err != nil {
		return nil, errors.Wrap(err, "Error in conf.LocalPicker.Get")
	}

	return p, nil
}

func (s *V1Instance) GetPeerList() []*PeerClient {
	s.peerMutex.RLock()
	defer s.peerMutex.RUnlock()
	return s.conf.LocalPicker.Peers()
}

func (s *V1Instance) GetRegionPickers() map[string]PeerPicker {
	s.peerMutex.RLock()
	defer s.peerMutex.RUnlock()
	return s.conf.RegionPicker.Pickers()
}

// Describe fetches prometheus metrics to be registered
func (s *V1Instance) Describe(ch chan<- *prometheus.Desc) {
	metricBatchQueueLength.Describe(ch)
	metricBatchSendDuration.Describe(ch)
	metricBatchSendRetries.Describe(ch)
	metricCheckErrorCounter.Describe(ch)
	metricCommandCounter.Describe(ch)
	metricConcurrentChecks.Describe(ch)
	metricFuncTimeDuration.Describe(ch)
	metricGetRateLimitCounter.Describe(ch)
	metricOverLimitCounter.Describe(ch)
	metricWorkerQueue.Describe(ch)
	metricUpdatePeerGlobalsCounter.Describe(ch)
	s.global.metricBroadcastDuration.Describe(ch)
	s.global.metricBroadcastErrors.Describe(ch)
	s.global.metricGlobalQueueLength.Describe(ch)
	s.global.metricGlobalSendDuration.Describe(ch)
	s.global.metricGlobalSendQueueLength.Describe(ch)
	s.global.metricGlobalSendErrors.Describe(ch)
}

// Collect fetches metrics from the server for use by prometheus
func (s *V1Instance) Collect(ch chan<- prometheus.Metric) {
	metricBatchQueueLength.Collect(ch)
	metricBatchSendDuration.Collect(ch)
	metricBatchSendRetries.Collect(ch)
	metricCheckErrorCounter.Collect(ch)
	metricCommandCounter.Collect(ch)
	metricConcurrentChecks.Collect(ch)
	metricFuncTimeDuration.Collect(ch)
	metricGetRateLimitCounter.Collect(ch)
	metricOverLimitCounter.Collect(ch)
	metricWorkerQueue.Collect(ch)
	metricUpdatePeerGlobalsCounter.Collect(ch)
	s.global.metricBroadcastDuration.Collect(ch)
	s.global.metricBroadcastErrors.Collect(ch)
	s.global.metricGlobalQueueLength.Collect(ch)
	s.global.metricGlobalSendDuration.Collect(ch)
	s.global.metricGlobalSendErrors.Collect(ch)
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

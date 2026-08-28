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

// Package envoy serves Envoy's RateLimitService (envoy.service.ratelimit.v3)
// on top of a gubernator instance. Envoy decides how much (the limit travels
// on each descriptor); gubernator decides how to count (the domain policy).
package envoy

import (
	"context"
	"math"
	"sort"
	"strings"
	"time"

	commonv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/common/ratelimit/v3"
	ratelimitv3 "github.com/envoyproxy/go-control-plane/envoy/service/ratelimit/v3"
	typev3 "github.com/envoyproxy/go-control-plane/envoy/type/v3"
	guber "github.com/gubernator-io/gubernator/v2"
	"github.com/mailgun/holster/v4/clock"
	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"
)

// maxDescriptors caps descriptors per call; it matches gubernator's batch limit.
const maxDescriptors = 1000

// Reference-implementation window lengths in milliseconds; a window starts at first hit.
var unitDurations = map[typev3.RateLimitUnit]int64{
	typev3.RateLimitUnit_SECOND: guber.Second,
	typev3.RateLimitUnit_MINUTE: guber.Minute,
	typev3.RateLimitUnit_HOUR:   60 * guber.Minute,
	typev3.RateLimitUnit_DAY:    24 * 60 * guber.Minute,
	typev3.RateLimitUnit_MONTH:  30 * 24 * 60 * guber.Minute,
	typev3.RateLimitUnit_YEAR:   365 * 24 * 60 * guber.Minute,
}

// Calendar interval codes used when the domain policy sets DURATION_IS_GREGORIAN.
var gregorianUnits = map[typev3.RateLimitUnit]int64{
	typev3.RateLimitUnit_MINUTE: guber.GregorianMinutes,
	typev3.RateLimitUnit_HOUR:   guber.GregorianHours,
	typev3.RateLimitUnit_DAY:    guber.GregorianDays,
	typev3.RateLimitUnit_MONTH:  guber.GregorianMonths,
	typev3.RateLimitUnit_YEAR:   guber.GregorianYears,
}

// Service implements ratelimitv3.RateLimitServiceServer and prometheus.Collector.
type Service struct {
	instance *guber.V1Instance

	metricRequests     *prometheus.CounterVec
	metricMissingLimit *prometheus.CounterVec
	metricDuration     prometheus.Histogram
}

var _ ratelimitv3.RateLimitServiceServer = (*Service)(nil)

// New returns a Service that evaluates descriptors through instance.
func New(instance *guber.V1Instance) *Service {
	return &Service{
		instance: instance,
		metricRequests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gubernator_envoy_rls_requests_total",
			Help: "The count of ShouldRateLimit calls by domain and overall code; failed calls carry the gRPC code.",
		}, []string{"domain", "code"}),
		metricMissingLimit: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gubernator_envoy_rls_missing_limit_total",
			Help: "The count of descriptors that arrived with no limit override, by domain and the policy action taken.",
		}, []string{"domain", "action"}),
		metricDuration: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name: "gubernator_envoy_rls_duration_seconds",
			Help: "The duration of ShouldRateLimit calls in seconds.",
		}),
	}
}

// Register is the gubernator.RegisterRLSFunc for this package: it registers
// the RateLimitService on every server and returns the metrics collector.
func Register(servers []*grpc.Server, instance *guber.V1Instance) prometheus.Collector {
	s := New(instance)
	for _, srv := range servers {
		ratelimitv3.RegisterRateLimitServiceServer(srv, s)
	}
	return s
}

// Describe implements prometheus.Collector
func (s *Service) Describe(ch chan<- *prometheus.Desc) {
	s.metricRequests.Describe(ch)
	s.metricMissingLimit.Describe(ch)
	s.metricDuration.Describe(ch)
}

// Collect implements prometheus.Collector
func (s *Service) Collect(ch chan<- prometheus.Metric) {
	s.metricRequests.Collect(ch)
	s.metricMissingLimit.Collect(ch)
	s.metricDuration.Collect(ch)
}

// pending is one descriptor's slot in the response. Descriptors handled by
// policy (no limit) fill status directly; the rest wait on the batch.
type pending struct {
	status *ratelimitv3.RateLimitResponse_DescriptorStatus
	limit  *commonv3.RateLimitDescriptor_RateLimitOverride
	index  int
}

// ShouldRateLimit evaluates every descriptor as one gubernator rate limit and
// reports OVER_LIMIT if any one is. Anything gubernator cannot evaluate fails
// the whole call so Envoy's failure_mode_deny decides.
func (s *Service) ShouldRateLimit(ctx context.Context, r *ratelimitv3.RateLimitRequest) (resp *ratelimitv3.RateLimitResponse, err error) {
	defer prometheus.NewTimer(s.metricDuration).ObserveDuration()
	defer func() {
		code := status.Code(err).String()
		if err == nil {
			code = resp.OverallCode.String()
		}
		s.metricRequests.WithLabelValues(r.Domain, code).Inc()
	}()

	if r.Domain == "" {
		return nil, status.Error(codes.InvalidArgument, "domain is required")
	}
	if len(r.Descriptors) == 0 {
		return nil, status.Error(codes.InvalidArgument, "no descriptors")
	}
	if len(r.Descriptors) > maxDescriptors {
		return nil, status.Errorf(codes.OutOfRange, "descriptors list too large; max size is '%d'", maxDescriptors)
	}

	policy := s.instance.ResolveEnvoyPolicy(r.Domain)
	gregorian := guber.HasBehavior(guber.Behavior(policy.Behavior), guber.Behavior_DURATION_IS_GREGORIAN)
	slots := make([]*pending, len(r.Descriptors))
	var batch []*guber.RateLimitReq
	var batched []*pending

	for i, d := range r.Descriptors {
		if len(d.Entries) == 0 {
			return nil, status.Errorf(codes.InvalidArgument, "domain %q descriptor has no entries", r.Domain)
		}
		name, key := identity(r.Domain, d.Entries)
		slot := &pending{index: i}
		slots[i] = slot

		if d.Limit == nil || d.Limit.Unit == typev3.RateLimitUnit_UNKNOWN || d.Limit.RequestsPerUnit == 0 {
			switch policy.OnMissingLimit {
			case guber.MissingLimitAction_ALLOW:
				s.metricMissingLimit.WithLabelValues(r.Domain, "allow").Inc()
				slot.status = &ratelimitv3.RateLimitResponse_DescriptorStatus{Code: ratelimitv3.RateLimitResponse_OK}
			case guber.MissingLimitAction_ERROR:
				s.metricMissingLimit.WithLabelValues(r.Domain, "error").Inc()
				return nil, status.Errorf(codes.InvalidArgument, "domain %q descriptor %s has no limit",
					r.Domain, strings.TrimPrefix(name, r.Domain+"."))
			default:
				s.metricMissingLimit.WithLabelValues(r.Domain, "deny").Inc()
				slot.status = &ratelimitv3.RateLimitResponse_DescriptorStatus{Code: ratelimitv3.RateLimitResponse_OVER_LIMIT}
			}
			continue
		}

		duration, ok := unitDurations[d.Limit.Unit]
		if gregorian {
			duration, ok = gregorianUnits[d.Limit.Unit]
		}
		if !ok {
			return nil, status.Errorf(codes.InvalidArgument, "domain %q descriptor %s: unit %s is not valid for this domain's behavior",
				r.Domain, strings.TrimPrefix(name, r.Domain+"."), d.Limit.Unit)
		}

		// Descriptor addend wins when set (zero included), then the request addend, then one
		hits := int64(1)
		if d.HitsAddend != nil {
			hits = int64(d.HitsAddend.Value)
		} else if r.HitsAddend != 0 {
			hits = int64(r.HitsAddend)
		}
		if d.IsNegativeHits {
			hits = -hits
		}

		slot.limit = d.Limit
		batched = append(batched, slot)
		batch = append(batch, &guber.RateLimitReq{
			Name:      name,
			UniqueKey: key,
			Hits:      hits,
			Limit:     int64(d.Limit.RequestsPerUnit),
			Duration:  duration,
			Algorithm: policy.Algorithm,
			Behavior:  guber.Behavior(policy.Behavior),
		})
	}

	if len(batch) > 0 {
		results, err := s.instance.GetRateLimits(ctx, &guber.GetRateLimitsReq{Requests: batch})
		if err != nil {
			return nil, status.Errorf(codes.Internal, "while evaluating rate limits: %s", err)
		}
		now := clock.Now().UnixMilli()
		for i, rl := range results.Responses {
			if rl.Error != "" {
				return nil, status.Errorf(codes.Internal, "domain %q descriptor %s: %s",
					r.Domain, strings.TrimPrefix(batch[i].Name, r.Domain+"."), rl.Error)
			}
			batched[i].status = toStatus(rl, batched[i].limit, now)
		}
	}

	resp = &ratelimitv3.RateLimitResponse{
		OverallCode: ratelimitv3.RateLimitResponse_OK,
		Statuses:    make([]*ratelimitv3.RateLimitResponse_DescriptorStatus, len(slots)),
	}
	for _, slot := range slots {
		resp.Statuses[slot.index] = slot.status
		if slot.status.Code == ratelimitv3.RateLimitResponse_OVER_LIMIT {
			resp.OverallCode = ratelimitv3.RateLimitResponse_OVER_LIMIT
		}
	}
	return resp, nil
}

// identity derives the gubernator name and unique key for a descriptor. Entries
// are stably sorted by key so the same entry set lands in one bucket regardless
// of the order Envoy sends them.
func identity(domain string, entries []*commonv3.RateLimitDescriptor_Entry) (name, key string) {
	sorted := make([]*commonv3.RateLimitDescriptor_Entry, len(entries))
	copy(sorted, entries)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Key < sorted[j].Key })

	keys := make([]string, 0, len(sorted))
	values := make([]string, 0, len(sorted))
	for _, e := range sorted {
		keys = append(keys, e.Key)
		values = append(values, e.Value)
	}
	return domain + "." + strings.Join(keys, "."), strings.Join(values, "|")
}

func toStatus(rl *guber.RateLimitResp, limit *commonv3.RateLimitDescriptor_RateLimitOverride, now int64) *ratelimitv3.RateLimitResponse_DescriptorStatus {
	st := &ratelimitv3.RateLimitResponse_DescriptorStatus{
		Code: ratelimitv3.RateLimitResponse_OK,
		CurrentLimit: &ratelimitv3.RateLimitResponse_RateLimit{
			RequestsPerUnit: limit.RequestsPerUnit,
			Unit:            ratelimitv3.RateLimitResponse_RateLimit_Unit(limit.Unit),
		},
		LimitRemaining:     uint32(min(max(rl.Remaining, 0), math.MaxUint32)),
		DurationUntilReset: durationpb.New(time.Duration(max(rl.ResetTime-now, 0)) * time.Millisecond),
	}
	if rl.Status == guber.Status_OVER_LIMIT {
		st.Code = ratelimitv3.RateLimitResponse_OVER_LIMIT
	}
	return st
}

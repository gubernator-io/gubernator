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

package envoy_test

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	commonv3 "github.com/envoyproxy/go-control-plane/envoy/extensions/common/ratelimit/v3"
	ratelimitv3 "github.com/envoyproxy/go-control-plane/envoy/service/ratelimit/v3"
	typev3 "github.com/envoyproxy/go-control-plane/envoy/type/v3"
	guber "github.com/gubernator-io/gubernator/v2"
	"github.com/gubernator-io/gubernator/v2/cluster"
	"github.com/gubernator-io/gubernator/v2/envoy"
	"github.com/mailgun/holster/v4/clock"
	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

const syncInterval = time.Second

func TestMain(m *testing.M) {
	var peers []guber.PeerInfo
	port := 2111
	for i := 0; i < 3; i++ {
		peers = append(peers, guber.PeerInfo{
			HTTPAddress: fmt.Sprintf("localhost:%d", port),
			GRPCAddress: fmt.Sprintf("localhost:%d", port+1),
		})
		port += 2
	}
	err := cluster.StartWith(peers, cluster.WithEnvoy(guber.EnvoyConfig{
		Enabled:            true,
		PolicySyncInterval: syncInterval,
		RegisterRLS:        envoy.Register,
	}))
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
	code := m.Run()
	cluster.Stop()
	os.Exit(code)
}

func dial(t *testing.T, addr string) *grpc.ClientConn {
	t.Helper()
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func rlsClient(t *testing.T, addr string) ratelimitv3.RateLimitServiceClient {
	return ratelimitv3.NewRateLimitServiceClient(dial(t, addr))
}

func policyClient(t *testing.T, addr string) guber.EnvoyPolicyV1Client {
	return guber.NewEnvoyPolicyV1Client(dial(t, addr))
}

func peersClient(t *testing.T, addr string) guber.PeersV1Client {
	return guber.NewPeersV1Client(dial(t, addr))
}

func entry(key, value string) *commonv3.RateLimitDescriptor_Entry {
	return &commonv3.RateLimitDescriptor_Entry{Key: key, Value: value}
}

func limited(requests uint32, unit typev3.RateLimitUnit, entries ...*commonv3.RateLimitDescriptor_Entry) *commonv3.RateLimitDescriptor {
	return &commonv3.RateLimitDescriptor{
		Entries: entries,
		Limit:   &commonv3.RateLimitDescriptor_RateLimitOverride{RequestsPerUnit: requests, Unit: unit},
	}
}

func unlimited(entries ...*commonv3.RateLimitDescriptor_Entry) *commonv3.RateLimitDescriptor {
	return &commonv3.RateLimitDescriptor{Entries: entries}
}

// metricValue returns the sample value of the metric whose name and labels match, or 0 when absent.
func metricValue(t *testing.T, httpAddr, name string, labels map[string]string) float64 {
	t.Helper()
	resp, err := http.Get(fmt.Sprintf("http://%s/metrics", httpAddr))
	require.NoError(t, err)
	defer resp.Body.Close()
	dec := expfmt.SampleDecoder{
		Dec:  expfmt.NewDecoder(resp.Body, expfmt.FmtText),
		Opts: &expfmt.DecodeOptions{Timestamp: model.Now()},
	}
	for {
		var samples model.Vector
		err := dec.Decode(&samples)
		if err == io.EOF {
			return 0
		}
		require.NoError(t, err)
	next:
		for _, s := range samples {
			if string(s.Metric[model.MetricNameLabel]) != name {
				continue
			}
			for k, v := range labels {
				if string(s.Metric[model.LabelName(k)]) != v {
					continue next
				}
			}
			return float64(s.Value)
		}
	}
}

func listPolicy(t *testing.T, addr, domain string) *guber.DomainPolicy {
	t.Helper()
	resp, err := policyClient(t, addr).ListPolicies(context.Background(), &guber.ListPoliciesReq{})
	require.NoError(t, err)
	for _, p := range resp.Policies {
		if p.Domain == domain {
			return p
		}
	}
	return nil
}

// requireConverged waits until every peer in the cluster reports the same entry for domain.
func requireConverged(t *testing.T, domain string, want *guber.DomainPolicy) {
	t.Helper()
	for _, peer := range cluster.GetPeers() {
		require.Eventually(t, func() bool {
			got := listPolicy(t, peer.GRPCAddress, domain)
			if want == nil || got == nil {
				return want == got
			}
			return got.Version == want.Version && got.Origin == want.Origin &&
				got.Algorithm == want.Algorithm && got.Behavior == want.Behavior &&
				got.OnMissingLimit == want.OnMissingLimit
		}, 5*time.Second, 50*time.Millisecond, "peer %s did not converge on %v", peer.GRPCAddress, want)
	}
}

func TestDisabledReturnsUnimplemented(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	d, err := guber.SpawnDaemon(ctx, guber.DaemonConfig{
		GRPCListenAddress: "127.0.0.1:0",
		HTTPListenAddress: "127.0.0.1:0",
	})
	require.NoError(t, err)
	defer d.Close()
	addr := d.GRPCListeners[0].Addr().String()

	_, err = rlsClient(t, addr).ShouldRateLimit(ctx, &ratelimitv3.RateLimitRequest{
		Domain:      "disabled",
		Descriptors: []*commonv3.RateLimitDescriptor{limited(1, typev3.RateLimitUnit_MINUTE, entry("k", "v"))},
	})
	assert.Equal(t, codes.Unimplemented, status.Code(err))

	_, err = policyClient(t, addr).ListPolicies(ctx, &guber.ListPoliciesReq{})
	assert.Equal(t, codes.Unimplemented, status.Code(err))

	_, err = policyClient(t, addr).ApplyPolicies(ctx, &guber.ApplyPoliciesReq{
		Policies: []*guber.DomainPolicy{{Domain: "disabled"}},
	})
	assert.Equal(t, codes.Unimplemented, status.Code(err))

	// The V1 API is unaffected by the flag
	v1, err := guber.DialV1Server(addr, nil)
	require.NoError(t, err)
	_, err = v1.LiveCheck(ctx, &guber.LiveCheckReq{})
	assert.NoError(t, err)
}

func TestOverLimitAfterLimitIsConsumed(t *testing.T) {
	domain := uniqueDomain(t)
	client := rlsClient(t, cluster.GetRandomPeer(cluster.DataCenterNone).GRPCAddress)
	req := &ratelimitv3.RateLimitRequest{
		Domain:      domain,
		Descriptors: []*commonv3.RateLimitDescriptor{limited(2, typev3.RateLimitUnit_MINUTE, entry("path", "/login"))},
	}

	for _, test := range []struct {
		code      ratelimitv3.RateLimitResponse_Code
		remaining uint32
	}{
		{code: ratelimitv3.RateLimitResponse_OK, remaining: 1},
		{code: ratelimitv3.RateLimitResponse_OK, remaining: 0},
		{code: ratelimitv3.RateLimitResponse_OVER_LIMIT, remaining: 0},
	} {
		resp, err := client.ShouldRateLimit(context.Background(), req)
		require.NoError(t, err)
		require.Len(t, resp.Statuses, 1)
		assert.Equal(t, test.code, resp.OverallCode)
		assert.Equal(t, test.code, resp.Statuses[0].Code)
		assert.Equal(t, test.remaining, resp.Statuses[0].LimitRemaining)
		require.NotNil(t, resp.Statuses[0].CurrentLimit)
		assert.Equal(t, uint32(2), resp.Statuses[0].CurrentLimit.RequestsPerUnit)
		assert.Equal(t, ratelimitv3.RateLimitResponse_RateLimit_MINUTE, resp.Statuses[0].CurrentLimit.Unit)
		require.NotNil(t, resp.Statuses[0].DurationUntilReset)
		until := resp.Statuses[0].DurationUntilReset.AsDuration()
		assert.Greater(t, until, time.Duration(0))
		assert.LessOrEqual(t, until, time.Minute)
	}
}

func TestEntryOrderDoesNotChangeBucket(t *testing.T) {
	domain := uniqueDomain(t)
	client := rlsClient(t, cluster.GetRandomPeer(cluster.DataCenterNone).GRPCAddress)

	first, err := client.ShouldRateLimit(context.Background(), &ratelimitv3.RateLimitRequest{
		Domain: domain,
		Descriptors: []*commonv3.RateLimitDescriptor{
			limited(10, typev3.RateLimitUnit_HOUR, entry("path", "/a"), entry("remote_address", "10.0.0.1")),
		},
	})
	require.NoError(t, err)
	assert.Equal(t, uint32(9), first.Statuses[0].LimitRemaining)

	second, err := client.ShouldRateLimit(context.Background(), &ratelimitv3.RateLimitRequest{
		Domain: domain,
		Descriptors: []*commonv3.RateLimitDescriptor{
			limited(10, typev3.RateLimitUnit_HOUR, entry("remote_address", "10.0.0.1"), entry("path", "/a")),
		},
	})
	require.NoError(t, err)
	assert.Equal(t, uint32(8), second.Statuses[0].LimitRemaining)
}

func TestSecondDescriptorOverLimitFailsCallAndConsumesFirst(t *testing.T) {
	domain := uniqueDomain(t)
	client := rlsClient(t, cluster.GetRandomPeer(cluster.DataCenterNone).GRPCAddress)
	req := &ratelimitv3.RateLimitRequest{
		Domain: domain,
		Descriptors: []*commonv3.RateLimitDescriptor{
			limited(10, typev3.RateLimitUnit_HOUR, entry("path", "/a")),
			limited(1, typev3.RateLimitUnit_HOUR, entry("user", "bob")),
		},
	}

	first, err := client.ShouldRateLimit(context.Background(), req)
	require.NoError(t, err)
	assert.Equal(t, ratelimitv3.RateLimitResponse_OK, first.OverallCode)
	assert.Equal(t, uint32(9), first.Statuses[0].LimitRemaining)
	assert.Equal(t, uint32(0), first.Statuses[1].LimitRemaining)

	second, err := client.ShouldRateLimit(context.Background(), req)
	require.NoError(t, err)
	assert.Equal(t, ratelimitv3.RateLimitResponse_OVER_LIMIT, second.OverallCode)
	require.Len(t, second.Statuses, 2)
	assert.Equal(t, ratelimitv3.RateLimitResponse_OK, second.Statuses[0].Code)
	assert.Equal(t, uint32(8), second.Statuses[0].LimitRemaining)
	assert.Equal(t, ratelimitv3.RateLimitResponse_OVER_LIMIT, second.Statuses[1].Code)
}

func TestNegativeHitsBankCreditAboveLimit(t *testing.T) {
	domain := uniqueDomain(t)
	client := rlsClient(t, cluster.GetRandomPeer(cluster.DataCenterNone).GRPCAddress)
	desc := limited(2, typev3.RateLimitUnit_MINUTE, entry("path", "/refund"))
	desc.HitsAddend = wrapperspb.UInt64(1)
	desc.IsNegativeHits = true

	resp, err := client.ShouldRateLimit(context.Background(), &ratelimitv3.RateLimitRequest{
		Domain:      domain,
		Descriptors: []*commonv3.RateLimitDescriptor{desc},
	})
	require.NoError(t, err)
	assert.Equal(t, ratelimitv3.RateLimitResponse_OK, resp.OverallCode)
	assert.Equal(t, uint32(3), resp.Statuses[0].LimitRemaining)
}

func TestRequestHitsAddendAppliesToEveryDescriptor(t *testing.T) {
	domain := uniqueDomain(t)
	client := rlsClient(t, cluster.GetRandomPeer(cluster.DataCenterNone).GRPCAddress)
	perDescriptor := limited(10, typev3.RateLimitUnit_MINUTE, entry("path", "/b"))
	perDescriptor.HitsAddend = wrapperspb.UInt64(2)

	resp, err := client.ShouldRateLimit(context.Background(), &ratelimitv3.RateLimitRequest{
		Domain:     domain,
		HitsAddend: 3,
		Descriptors: []*commonv3.RateLimitDescriptor{
			limited(10, typev3.RateLimitUnit_MINUTE, entry("path", "/a")),
			perDescriptor,
		},
	})
	require.NoError(t, err)
	// The request-level addend applies where the descriptor sets none
	assert.Equal(t, uint32(7), resp.Statuses[0].LimitRemaining)
	// The descriptor-level addend wins over the request-level one
	assert.Equal(t, uint32(8), resp.Statuses[1].LimitRemaining)
}

func TestMissingLimitDefaultsToDeny(t *testing.T) {
	domain := uniqueDomain(t)
	peer := cluster.GetRandomPeer(cluster.DataCenterNone)
	client := rlsClient(t, peer.GRPCAddress)
	before := metricValue(t, peer.HTTPAddress, "gubernator_envoy_rls_missing_limit_total",
		map[string]string{"domain": domain, "action": "deny"})

	resp, err := client.ShouldRateLimit(context.Background(), &ratelimitv3.RateLimitRequest{
		Domain:      domain,
		Descriptors: []*commonv3.RateLimitDescriptor{unlimited(entry("path", "/nolimit"))},
	})
	require.NoError(t, err)
	assert.Equal(t, ratelimitv3.RateLimitResponse_OVER_LIMIT, resp.OverallCode)
	require.Len(t, resp.Statuses, 1)
	assert.Equal(t, ratelimitv3.RateLimitResponse_OVER_LIMIT, resp.Statuses[0].Code)
	assert.Nil(t, resp.Statuses[0].CurrentLimit)
	assert.Equal(t, uint32(0), resp.Statuses[0].LimitRemaining)

	after := metricValue(t, peer.HTTPAddress, "gubernator_envoy_rls_missing_limit_total",
		map[string]string{"domain": domain, "action": "deny"})
	assert.Equal(t, before+1, after)
}

func TestMissingLimitFollowsDomainPolicy(t *testing.T) {
	domain := uniqueDomain(t)
	peer := cluster.GetRandomPeer(cluster.DataCenterNone)
	client := rlsClient(t, peer.GRPCAddress)
	policies := policyClient(t, peer.GRPCAddress)
	req := &ratelimitv3.RateLimitRequest{
		Domain:      domain,
		Descriptors: []*commonv3.RateLimitDescriptor{unlimited(entry("path", "/nolimit"))},
	}

	_, err := policies.ApplyPolicies(context.Background(), &guber.ApplyPoliciesReq{
		Policies: []*guber.DomainPolicy{{Domain: domain, OnMissingLimit: guber.MissingLimitAction_ALLOW}},
	})
	require.NoError(t, err)

	resp, err := client.ShouldRateLimit(context.Background(), req)
	require.NoError(t, err)
	assert.Equal(t, ratelimitv3.RateLimitResponse_OK, resp.OverallCode)
	assert.Equal(t, ratelimitv3.RateLimitResponse_OK, resp.Statuses[0].Code)
	assert.Nil(t, resp.Statuses[0].CurrentLimit)
	assert.Equal(t, float64(1), metricValue(t, peer.HTTPAddress, "gubernator_envoy_rls_missing_limit_total",
		map[string]string{"domain": domain, "action": "allow"}))

	_, err = policies.ApplyPolicies(context.Background(), &guber.ApplyPoliciesReq{
		Policies: []*guber.DomainPolicy{{Domain: domain, OnMissingLimit: guber.MissingLimitAction_ERROR}},
	})
	require.NoError(t, err)

	_, err = client.ShouldRateLimit(context.Background(), req)
	require.Error(t, err)
	assert.Equal(t, codes.InvalidArgument, status.Code(err))
	assert.ErrorContains(t, err, domain)
	assert.ErrorContains(t, err, "has no limit")
	assert.Equal(t, float64(1), metricValue(t, peer.HTTPAddress, "gubernator_envoy_rls_missing_limit_total",
		map[string]string{"domain": domain, "action": "error"}))
}

func TestGregorianMinuteResetsAtClockBoundary(t *testing.T) {
	domain := uniqueDomain(t)
	now := clock.Date(2026, clock.March, 3, 12, 0, 20, 0, clock.UTC)
	defer clock.Freeze(now).Unfreeze()
	peer := cluster.GetRandomPeer(cluster.DataCenterNone)

	_, err := policyClient(t, peer.GRPCAddress).ApplyPolicies(context.Background(), &guber.ApplyPoliciesReq{
		Policies: []*guber.DomainPolicy{{Domain: domain, Behavior: int32(guber.Behavior_DURATION_IS_GREGORIAN)}},
	})
	require.NoError(t, err)

	resp, err := rlsClient(t, peer.GRPCAddress).ShouldRateLimit(context.Background(), &ratelimitv3.RateLimitRequest{
		Domain:      domain,
		Descriptors: []*commonv3.RateLimitDescriptor{limited(5, typev3.RateLimitUnit_MINUTE, entry("path", "/a"))},
	})
	require.NoError(t, err)
	assert.Equal(t, ratelimitv3.RateLimitResponse_OK, resp.OverallCode)
	// 40s remain in the current clock minute; the boundary is one millisecond short of the next minute
	assert.InDelta(t, (40 * time.Second).Milliseconds(), resp.Statuses[0].DurationUntilReset.AsDuration().Milliseconds(), 1)
}

func TestGregorianSecondIsRejected(t *testing.T) {
	domain := uniqueDomain(t)
	peer := cluster.GetRandomPeer(cluster.DataCenterNone)
	_, err := policyClient(t, peer.GRPCAddress).ApplyPolicies(context.Background(), &guber.ApplyPoliciesReq{
		Policies: []*guber.DomainPolicy{{Domain: domain, Behavior: int32(guber.Behavior_DURATION_IS_GREGORIAN)}},
	})
	require.NoError(t, err)

	_, err = rlsClient(t, peer.GRPCAddress).ShouldRateLimit(context.Background(), &ratelimitv3.RateLimitRequest{
		Domain:      domain,
		Descriptors: []*commonv3.RateLimitDescriptor{limited(5, typev3.RateLimitUnit_SECOND, entry("path", "/a"))},
	})
	require.Error(t, err)
	assert.Equal(t, codes.InvalidArgument, status.Code(err))
}

func TestMonthAndYearArePlainDurations(t *testing.T) {
	domain := uniqueDomain(t)
	defer clock.Freeze(clock.Now()).Unfreeze()
	client := rlsClient(t, cluster.GetRandomPeer(cluster.DataCenterNone).GRPCAddress)
	req := &ratelimitv3.RateLimitRequest{
		Domain: domain,
		Descriptors: []*commonv3.RateLimitDescriptor{
			limited(100, typev3.RateLimitUnit_MONTH, entry("path", "/month")),
			limited(100, typev3.RateLimitUnit_YEAR, entry("path", "/year")),
		},
	}
	const day = 24 * time.Hour

	first, err := client.ShouldRateLimit(context.Background(), req)
	require.NoError(t, err)
	assert.Equal(t, 30*day, first.Statuses[0].DurationUntilReset.AsDuration())
	assert.Equal(t, 365*day, first.Statuses[1].DurationUntilReset.AsDuration())

	clock.Advance(time.Hour)
	second, err := client.ShouldRateLimit(context.Background(), req)
	require.NoError(t, err)
	assert.Equal(t, 30*day-time.Hour, second.Statuses[0].DurationUntilReset.AsDuration())
	assert.Equal(t, 365*day-time.Hour, second.Statuses[1].DurationUntilReset.AsDuration())
}

func TestLeakyBucketPolicyIsApplied(t *testing.T) {
	domain := uniqueDomain(t)
	peer := cluster.GetRandomPeer(cluster.DataCenterNone)
	_, err := policyClient(t, peer.GRPCAddress).ApplyPolicies(context.Background(), &guber.ApplyPoliciesReq{
		Policies: []*guber.DomainPolicy{{Domain: domain, Algorithm: guber.Algorithm_LEAKY_BUCKET}},
	})
	require.NoError(t, err)

	client := rlsClient(t, peer.GRPCAddress)
	req := &ratelimitv3.RateLimitRequest{
		Domain:      domain,
		Descriptors: []*commonv3.RateLimitDescriptor{limited(2, typev3.RateLimitUnit_MINUTE, entry("path", "/leaky"))},
	}
	for i, remaining := range []uint32{1, 0} {
		resp, err := client.ShouldRateLimit(context.Background(), req)
		require.NoError(t, err)
		assert.Equal(t, ratelimitv3.RateLimitResponse_OK, resp.OverallCode, "call %d", i)
		assert.Equal(t, remaining, resp.Statuses[0].LimitRemaining)
	}
	resp, err := client.ShouldRateLimit(context.Background(), req)
	require.NoError(t, err)
	assert.Equal(t, ratelimitv3.RateLimitResponse_OVER_LIMIT, resp.OverallCode)
}

func TestApplyPropagatesToEveryPeer(t *testing.T) {
	domain := uniqueDomain(t)
	peer := cluster.PeerAt(0)
	origin := cluster.DaemonAt(0).Config().AdvertiseAddress
	resp, err := policyClient(t, peer.GRPCAddress).ApplyPolicies(context.Background(), &guber.ApplyPoliciesReq{
		Policies: []*guber.DomainPolicy{{
			Domain:         domain,
			Algorithm:      guber.Algorithm_LEAKY_BUCKET,
			Behavior:       int32(guber.Behavior_GLOBAL),
			OnMissingLimit: guber.MissingLimitAction_ALLOW,
		}},
	})
	require.NoError(t, err)
	require.Len(t, resp.Applied, 1)
	assert.Empty(t, resp.UnreachablePeers)
	applied := resp.Applied[0]
	assert.NotZero(t, applied.Version)
	assert.Equal(t, origin, applied.Origin)
	assert.Equal(t, guber.Algorithm_LEAKY_BUCKET, applied.Algorithm)
	assert.Equal(t, int32(guber.Behavior_GLOBAL), applied.Behavior)
	assert.Equal(t, guber.MissingLimitAction_ALLOW, applied.OnMissingLimit)

	requireConverged(t, domain, applied)

	for _, d := range cluster.GetDaemons() {
		assert.Equal(t, float64(applied.Version), metricValue(t, d.PeerInfo.HTTPAddress,
			"gubernator_envoy_policy_version", map[string]string{"domain": domain}))
	}
}

func TestClientSuppliedVersionAndOriginAreIgnored(t *testing.T) {
	domain := uniqueDomain(t)
	peer := cluster.PeerAt(0)
	resp, err := policyClient(t, peer.GRPCAddress).ApplyPolicies(context.Background(), &guber.ApplyPoliciesReq{
		Policies: []*guber.DomainPolicy{{Domain: domain, Version: 1, Origin: "attacker"}},
	})
	require.NoError(t, err)
	assert.NotEqual(t, int64(1), resp.Applied[0].Version)
	assert.Equal(t, cluster.DaemonAt(0).Config().AdvertiseAddress, resp.Applied[0].Origin)
}

func TestApplyVersionIsStrictlyIncreasing(t *testing.T) {
	domain := uniqueDomain(t)
	now := clock.Now()
	defer clock.Freeze(now).Unfreeze()
	client := policyClient(t, cluster.PeerAt(0).GRPCAddress)
	apply := func() int64 {
		resp, err := client.ApplyPolicies(context.Background(), &guber.ApplyPoliciesReq{
			Policies: []*guber.DomainPolicy{{Domain: domain}},
		})
		require.NoError(t, err)
		return resp.Applied[0].Version
	}

	first := apply()
	assert.Equal(t, now.UnixMilli(), first)
	// Same millisecond: the ratchet still advances
	assert.Equal(t, first+1, apply())

	// A clock step backwards cannot regress the version
	clock.Freeze(now.Add(-time.Hour))
	assert.Equal(t, first+2, apply())
}

func TestLaterApplyOnAnotherPeerWins(t *testing.T) {
	domain := uniqueDomain(t)
	defer clock.Freeze(clock.Now()).Unfreeze()
	peerA, peerB := cluster.PeerAt(0), cluster.PeerAt(1)

	fromA, err := policyClient(t, peerA.GRPCAddress).ApplyPolicies(context.Background(), &guber.ApplyPoliciesReq{
		Policies: []*guber.DomainPolicy{{Domain: domain, OnMissingLimit: guber.MissingLimitAction_DENY}},
	})
	require.NoError(t, err)

	clock.Advance(time.Millisecond)
	fromB, err := policyClient(t, peerB.GRPCAddress).ApplyPolicies(context.Background(), &guber.ApplyPoliciesReq{
		Policies: []*guber.DomainPolicy{{Domain: domain, OnMissingLimit: guber.MissingLimitAction_ALLOW}},
	})
	require.NoError(t, err)
	assert.Greater(t, fromB.Applied[0].Version, fromA.Applied[0].Version)
	requireConverged(t, domain, fromB.Applied[0])

	// Apply on both again without advancing the clock; every peer picks the same winner
	againA, err := policyClient(t, peerA.GRPCAddress).ApplyPolicies(context.Background(), &guber.ApplyPoliciesReq{
		Policies: []*guber.DomainPolicy{{Domain: domain, OnMissingLimit: guber.MissingLimitAction_ERROR}},
	})
	require.NoError(t, err)
	againB, err := policyClient(t, peerB.GRPCAddress).ApplyPolicies(context.Background(), &guber.ApplyPoliciesReq{
		Policies: []*guber.DomainPolicy{{Domain: domain, OnMissingLimit: guber.MissingLimitAction_DENY}},
	})
	require.NoError(t, err)

	winner := againB.Applied[0]
	if againA.Applied[0].Version > winner.Version ||
		(againA.Applied[0].Version == winner.Version && againA.Applied[0].Origin > winner.Origin) {
		winner = againA.Applied[0]
	}
	requireConverged(t, domain, winner)
}

func TestEqualVersionsBreakTiesByOrigin(t *testing.T) {
	peer := cluster.PeerAt(2)
	peers := peersClient(t, peer.GRPCAddress)
	const version = 1_700_000_000_000

	for _, test := range []struct {
		name  string
		order []string
	}{
		{name: "HigherOriginFirst", order: []string{"z-peer", "a-peer"}},
		{name: "LowerOriginFirst", order: []string{"a-peer", "z-peer"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			domain := uniqueDomain(t)
			for _, origin := range test.order {
				_, err := peers.UpdatePeerPolicies(context.Background(), &guber.UpdatePeerPoliciesReq{
					Policies: []*guber.PeerPolicy{{Policy: &guber.DomainPolicy{
						Domain:  domain,
						Version: version,
						Origin:  origin,
						// Encode the origin in the policy so the winner is observable
						OnMissingLimit: map[string]guber.MissingLimitAction{
							"z-peer": guber.MissingLimitAction_ALLOW,
							"a-peer": guber.MissingLimitAction_ERROR,
						}[origin],
					}}},
				})
				require.NoError(t, err)
			}
			got := listPolicy(t, peer.GRPCAddress, domain)
			require.NotNil(t, got)
			assert.Equal(t, "z-peer", got.Origin)
			assert.Equal(t, guber.MissingLimitAction_ALLOW, got.OnMissingLimit)
		})
	}
}

func TestStalePeerUpdateIsDiscarded(t *testing.T) {
	domain := uniqueDomain(t)
	peer := cluster.PeerAt(2)
	applied, err := policyClient(t, peer.GRPCAddress).ApplyPolicies(context.Background(), &guber.ApplyPoliciesReq{
		Policies: []*guber.DomainPolicy{{Domain: domain, OnMissingLimit: guber.MissingLimitAction_ALLOW}},
	})
	require.NoError(t, err)
	current := applied.Applied[0]

	_, err = peersClient(t, peer.GRPCAddress).UpdatePeerPolicies(context.Background(), &guber.UpdatePeerPoliciesReq{
		Policies: []*guber.PeerPolicy{{Policy: &guber.DomainPolicy{
			Domain:         domain,
			Version:        current.Version - 1,
			Origin:         "zzz-any-origin",
			OnMissingLimit: guber.MissingLimitAction_ERROR,
		}}},
	})
	require.NoError(t, err)

	got := listPolicy(t, peer.GRPCAddress, domain)
	require.NotNil(t, got)
	assert.Equal(t, current.Version, got.Version)
	assert.Equal(t, current.Origin, got.Origin)
	assert.Equal(t, guber.MissingLimitAction_ALLOW, got.OnMissingLimit)
}

func TestDeleteSurvivesReplayOfOlderApply(t *testing.T) {
	domain := uniqueDomain(t)
	peerA, peerB := cluster.PeerAt(0), cluster.PeerAt(1)
	applied, err := policyClient(t, peerA.GRPCAddress).ApplyPolicies(context.Background(), &guber.ApplyPoliciesReq{
		Policies: []*guber.DomainPolicy{{Domain: domain, OnMissingLimit: guber.MissingLimitAction_ALLOW}},
	})
	require.NoError(t, err)
	requireConverged(t, domain, applied.Applied[0])

	deleted, err := policyClient(t, peerA.GRPCAddress).DeletePolicies(context.Background(), &guber.DeletePoliciesReq{
		Domains: []string{domain},
	})
	require.NoError(t, err)
	assert.Empty(t, deleted.UnreachablePeers)
	requireConverged(t, domain, nil)

	// Replay the original apply on another peer; the tombstone outranks it everywhere
	_, err = peersClient(t, peerB.GRPCAddress).UpdatePeerPolicies(context.Background(), &guber.UpdatePeerPoliciesReq{
		Policies: []*guber.PeerPolicy{{Policy: applied.Applied[0]}},
	})
	require.NoError(t, err)
	for _, peer := range cluster.GetPeers() {
		assert.Nil(t, listPolicy(t, peer.GRPCAddress, domain), "peer %s resurrected the domain", peer.GRPCAddress)
	}

	// The RLS falls back to the global default (deny) once the policy is gone
	resp, err := rlsClient(t, peerB.GRPCAddress).ShouldRateLimit(context.Background(), &ratelimitv3.RateLimitRequest{
		Domain:      domain,
		Descriptors: []*commonv3.RateLimitDescriptor{unlimited(entry("path", "/x"))},
	})
	require.NoError(t, err)
	assert.Equal(t, ratelimitv3.RateLimitResponse_OVER_LIMIT, resp.OverallCode)
}

func TestRestartedPeerPullsPolicyWithinSyncInterval(t *testing.T) {
	domain := uniqueDomain(t)
	restarted := cluster.DaemonAt(2)
	restarted.Close()

	peerA := cluster.PeerAt(0)
	applied, err := policyClient(t, peerA.GRPCAddress).ApplyPolicies(context.Background(), &guber.ApplyPoliciesReq{
		Policies: []*guber.DomainPolicy{{Domain: domain, OnMissingLimit: guber.MissingLimitAction_ALLOW}},
	})
	require.NoError(t, err)
	assert.Equal(t, []string{cluster.PeerAt(2).GRPCAddress}, applied.UnreachablePeers)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	require.NoError(t, restarted.Start(ctx))
	restarted.SetPeers(cluster.GetPeers())

	require.Eventually(t, func() bool {
		got := listPolicy(t, cluster.PeerAt(2).GRPCAddress, domain)
		return got != nil && got.Version == applied.Applied[0].Version
	}, 3*syncInterval, 50*time.Millisecond)
}

func TestInvalidPoliciesAreRejectedBeforeAnyApply(t *testing.T) {
	peer := cluster.PeerAt(0)
	client := policyClient(t, peer.GRPCAddress)
	valid := uniqueDomain(t) + "-valid"
	domain := uniqueDomain(t)

	for _, test := range []struct {
		name     string
		policies []*guber.DomainPolicy
		wantErr  string
	}{
		{
			name:     "EmptyDomain",
			policies: []*guber.DomainPolicy{{Domain: valid}, {Domain: ""}},
			wantErr:  "domain",
		},
		{
			name:     "UnknownAlgorithm",
			policies: []*guber.DomainPolicy{{Domain: valid}, {Domain: domain, Algorithm: 42}},
			wantErr:  "algorithm",
		},
		{
			name:     "UnknownMissingLimitAction",
			policies: []*guber.DomainPolicy{{Domain: valid}, {Domain: domain, OnMissingLimit: 42}},
			wantErr:  "on_missing_limit",
		},
		{
			name:     "UnknownBehaviorBit",
			policies: []*guber.DomainPolicy{{Domain: valid}, {Domain: domain, Behavior: 1 << 20}},
			wantErr:  "behavior",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := client.ApplyPolicies(context.Background(), &guber.ApplyPoliciesReq{Policies: test.policies})
			require.Error(t, err)
			assert.Equal(t, codes.InvalidArgument, status.Code(err))
			assert.ErrorContains(t, err, test.wantErr)
			assert.Nil(t, listPolicy(t, peer.GRPCAddress, valid))
		})
	}

	_, err := client.DeletePolicies(context.Background(), &guber.DeletePoliciesReq{Domains: []string{""}})
	assert.Equal(t, codes.InvalidArgument, status.Code(err))
}

func TestPeerUpdatesRequireVersionAndOrigin(t *testing.T) {
	domain := uniqueDomain(t)
	client := peersClient(t, cluster.PeerAt(0).GRPCAddress)
	for _, test := range []struct {
		name   string
		policy *guber.DomainPolicy
	}{
		{name: "ZeroVersion", policy: &guber.DomainPolicy{Domain: domain, Origin: "peer"}},
		{name: "EmptyOrigin", policy: &guber.DomainPolicy{Domain: domain, Version: 1}},
		{name: "EmptyDomain", policy: &guber.DomainPolicy{Version: 1, Origin: "peer"}},
		{name: "NoPolicy", policy: nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := client.UpdatePeerPolicies(context.Background(), &guber.UpdatePeerPoliciesReq{
				Policies: []*guber.PeerPolicy{{Policy: test.policy}},
			})
			require.Error(t, err)
			assert.Equal(t, codes.InvalidArgument, status.Code(err))
		})
	}
	assert.Nil(t, listPolicy(t, cluster.PeerAt(0).GRPCAddress, domain))
}

func TestGetPeerPoliciesIncludesTombstones(t *testing.T) {
	domain := uniqueDomain(t)
	peer := cluster.PeerAt(1)
	_, err := policyClient(t, peer.GRPCAddress).ApplyPolicies(context.Background(), &guber.ApplyPoliciesReq{
		Policies: []*guber.DomainPolicy{{Domain: domain}},
	})
	require.NoError(t, err)
	_, err = policyClient(t, peer.GRPCAddress).DeletePolicies(context.Background(), &guber.DeletePoliciesReq{
		Domains: []string{domain},
	})
	require.NoError(t, err)

	resp, err := peersClient(t, peer.GRPCAddress).GetPeerPolicies(context.Background(), &guber.GetPeerPoliciesReq{})
	require.NoError(t, err)
	var found *guber.PeerPolicy
	for _, p := range resp.Policies {
		if p.Policy.Domain == domain {
			found = p
		}
	}
	require.NotNil(t, found)
	assert.True(t, found.Deleted)
	assert.NotZero(t, found.Policy.Version)
	assert.Equal(t, cluster.DaemonAt(1).Config().AdvertiseAddress, found.Policy.Origin)
	assert.Nil(t, listPolicy(t, peer.GRPCAddress, domain))
}

func TestInvalidDescriptorsFailTheWholeCall(t *testing.T) {
	domain := uniqueDomain(t)
	client := rlsClient(t, cluster.GetRandomPeer(cluster.DataCenterNone).GRPCAddress)

	_, err := client.ShouldRateLimit(context.Background(), &ratelimitv3.RateLimitRequest{
		Domain: domain,
		Descriptors: []*commonv3.RateLimitDescriptor{
			limited(2, typev3.RateLimitUnit_MINUTE, entry("path", "/a")),
			limited(2, typev3.RateLimitUnit_MINUTE),
		},
	})
	require.Error(t, err)
	assert.Equal(t, codes.InvalidArgument, status.Code(err))
	assert.ErrorContains(t, err, domain)
	assert.ErrorContains(t, err, "no entries")

	// The valid descriptor in the failed call created no bucket
	resp, err := client.ShouldRateLimit(context.Background(), &ratelimitv3.RateLimitRequest{
		Domain:      domain,
		Descriptors: []*commonv3.RateLimitDescriptor{limited(2, typev3.RateLimitUnit_MINUTE, entry("path", "/a"))},
	})
	require.NoError(t, err)
	assert.Equal(t, uint32(1), resp.Statuses[0].LimitRemaining)

	_, err = client.ShouldRateLimit(context.Background(), &ratelimitv3.RateLimitRequest{Domain: domain})
	require.Error(t, err)
	assert.Equal(t, codes.InvalidArgument, status.Code(err))
	assert.ErrorContains(t, err, "no descriptors")

	_, err = client.ShouldRateLimit(context.Background(), &ratelimitv3.RateLimitRequest{
		Descriptors: []*commonv3.RateLimitDescriptor{limited(2, typev3.RateLimitUnit_MINUTE, entry("path", "/a"))},
	})
	require.Error(t, err)
	assert.Equal(t, codes.InvalidArgument, status.Code(err))
}

func TestTooManyDescriptorsIsOutOfRange(t *testing.T) {
	domain := uniqueDomain(t)
	client := rlsClient(t, cluster.GetRandomPeer(cluster.DataCenterNone).GRPCAddress)
	var descriptors []*commonv3.RateLimitDescriptor
	for i := 0; i < 1001; i++ {
		descriptors = append(descriptors, unlimited(entry("i", fmt.Sprint(i))))
	}
	_, err := client.ShouldRateLimit(context.Background(), &ratelimitv3.RateLimitRequest{
		Domain:      domain,
		Descriptors: descriptors,
	})
	require.Error(t, err)
	assert.Equal(t, codes.OutOfRange, status.Code(err))
}

func TestRequestMetricsCountEveryCall(t *testing.T) {
	domain := uniqueDomain(t)
	peer := cluster.GetRandomPeer(cluster.DataCenterNone)
	client := rlsClient(t, peer.GRPCAddress)
	req := &ratelimitv3.RateLimitRequest{
		Domain:      domain,
		Descriptors: []*commonv3.RateLimitDescriptor{limited(1, typev3.RateLimitUnit_MINUTE, entry("path", "/m"))},
	}
	before := metricValue(t, peer.HTTPAddress, "gubernator_envoy_rls_duration_seconds_count", nil)
	_, err := client.ShouldRateLimit(context.Background(), req)
	require.NoError(t, err)
	_, err = client.ShouldRateLimit(context.Background(), req)
	require.NoError(t, err)

	assert.Equal(t, float64(1), metricValue(t, peer.HTTPAddress, "gubernator_envoy_rls_requests_total",
		map[string]string{"domain": domain, "code": "OK"}))
	assert.Equal(t, float64(1), metricValue(t, peer.HTTPAddress, "gubernator_envoy_rls_requests_total",
		map[string]string{"domain": domain, "code": "OVER_LIMIT"}))
	assert.GreaterOrEqual(t, metricValue(t, peer.HTTPAddress, "gubernator_envoy_rls_duration_seconds_count", nil), before+2)
}

func TestPolicyHTTPGateway(t *testing.T) {
	domain := uniqueDomain(t)
	peer := cluster.PeerAt(0)
	resp, err := http.Post(fmt.Sprintf("http://%s/v1/envoy/policies", peer.HTTPAddress), "application/json",
		stringsReader(fmt.Sprintf(`{"policies":[{"domain":%q,"algorithm":"LEAKY_BUCKET","on_missing_limit":"ALLOW"}]}`, domain)))
	require.NoError(t, err)
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
	assert.Contains(t, string(body), `"applied"`)

	list, err := http.Get(fmt.Sprintf("http://%s/v1/envoy/policies", peer.HTTPAddress))
	require.NoError(t, err)
	defer list.Body.Close()
	body, err = io.ReadAll(list.Body)
	require.NoError(t, err)
	assert.Contains(t, string(body), fmt.Sprintf(`"domain":%q`, domain))
	assert.Contains(t, string(body), `"LEAKY_BUCKET"`)

	del, err := http.Post(fmt.Sprintf("http://%s/v1/envoy/policies.delete", peer.HTTPAddress), "application/json",
		stringsReader(fmt.Sprintf(`{"domains":[%q]}`, domain)))
	require.NoError(t, err)
	defer del.Body.Close()
	assert.Equal(t, http.StatusOK, del.StatusCode)
	assert.Nil(t, listPolicy(t, peer.GRPCAddress, domain))
}

func TestShouldRateLimitOverTLS(t *testing.T) {
	domain := uniqueDomain(t)
	serverTLS := guber.TLSConfig{
		CaFile:     "../contrib/certs/ca.cert",
		CertFile:   "../contrib/certs/gubernator.pem",
		KeyFile:    "../contrib/certs/gubernator.key",
		ClientAuth: tls.RequireAndVerifyClientCert,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	const addr = "127.0.0.1:9795"
	d, err := guber.SpawnDaemon(ctx, guber.DaemonConfig{
		GRPCListenAddress: addr,
		HTTPListenAddress: "127.0.0.1:9785",
		AdvertiseAddress:  addr,
		TLS:               &serverTLS,
		Envoy:             guber.EnvoyConfig{Enabled: true, RegisterRLS: envoy.Register},
	})
	require.NoError(t, err)
	defer d.Close()
	d.SetPeers([]guber.PeerInfo{{GRPCAddress: addr}})

	// Without the client certificate the V1 API requires, the RLS is unreachable too
	_, err = rlsClient(t, addr).ShouldRateLimit(ctx, &ratelimitv3.RateLimitRequest{
		Domain:      domain,
		Descriptors: []*commonv3.RateLimitDescriptor{limited(2, typev3.RateLimitUnit_MINUTE, entry("k", "v"))},
	})
	require.Error(t, err)

	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(credentials.NewTLS(serverTLS.ClientTLS)))
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()
	resp, err := ratelimitv3.NewRateLimitServiceClient(conn).ShouldRateLimit(ctx, &ratelimitv3.RateLimitRequest{
		Domain:      domain,
		Descriptors: []*commonv3.RateLimitDescriptor{limited(2, typev3.RateLimitUnit_MINUTE, entry("k", "v"))},
	})
	require.NoError(t, err)
	assert.Equal(t, ratelimitv3.RateLimitResponse_OK, resp.OverallCode)
	assert.Equal(t, uint32(1), resp.Statuses[0].LimitRemaining)
}

func TestEnabledWithoutRegistrarFailsToStart(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := guber.SpawnDaemon(ctx, guber.DaemonConfig{
		GRPCListenAddress: "127.0.0.1:0",
		HTTPListenAddress: "127.0.0.1:0",
		Envoy:             guber.EnvoyConfig{Enabled: true},
	})
	require.Error(t, err)
	assert.ErrorContains(t, err, "RegisterRLS")
}

func stringsReader(s string) io.Reader { return strings.NewReader(s) }

// uniqueDomain keeps buckets and policies from one test run out of the next;
// the cluster outlives every run in the process.
func uniqueDomain(t *testing.T) string {
	return t.Name() + "-" + guber.RandomString(6)
}

func TestDescriptorHitsAddendZeroDoesNotConsume(t *testing.T) {
	domain := uniqueDomain(t)
	client := rlsClient(t, cluster.GetRandomPeer(cluster.DataCenterNone).GRPCAddress)
	peek := limited(5, typev3.RateLimitUnit_MINUTE, entry("path", "/peek"))
	peek.HitsAddend = wrapperspb.UInt64(0)

	resp, err := client.ShouldRateLimit(context.Background(), &ratelimitv3.RateLimitRequest{
		Domain:      domain,
		HitsAddend:  3,
		Descriptors: []*commonv3.RateLimitDescriptor{peek},
	})
	require.NoError(t, err)
	assert.Equal(t, ratelimitv3.RateLimitResponse_OK, resp.OverallCode)
	assert.Equal(t, uint32(5), resp.Statuses[0].LimitRemaining)
}

func TestRepeatedDomainInOneApplyKeepsTheLastEntry(t *testing.T) {
	domain := uniqueDomain(t)
	defer clock.Freeze(clock.Now()).Unfreeze()
	resp, err := policyClient(t, cluster.PeerAt(0).GRPCAddress).ApplyPolicies(context.Background(), &guber.ApplyPoliciesReq{
		Policies: []*guber.DomainPolicy{
			{Domain: domain, OnMissingLimit: guber.MissingLimitAction_ALLOW},
			{Domain: domain, OnMissingLimit: guber.MissingLimitAction_ERROR},
		},
	})
	require.NoError(t, err)
	require.Len(t, resp.Applied, 2)
	assert.Equal(t, resp.Applied[0].Version+1, resp.Applied[1].Version)
	requireConverged(t, domain, resp.Applied[1])
}

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

package gubernator_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	guber "github.com/gubernator-io/gubernator/v2"
	"github.com/gubernator-io/gubernator/v2/cluster"
	"github.com/mailgun/errors"
	"github.com/mailgun/holster/v4/clock"
	"github.com/mailgun/holster/v4/syncutil"
	"github.com/mailgun/holster/v4/testutil"
	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/exp/maps"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	json "google.golang.org/protobuf/encoding/protojson"
)

// Setup and shutdown the mock gubernator cluster for the entire test suite
func TestMain(m *testing.M) {
	err := startGubernator()
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}

	code := m.Run()
	cluster.Stop()

	// os.Exit doesn't run deferred functions
	os.Exit(code)
}

func TestOverTheLimit(t *testing.T) {
	client, err := guber.DialV1Server(cluster.GetRandomPeer(cluster.DataCenterNone).GRPCAddress, nil)
	require.NoError(t, err)

	tests := []struct {
		Remaining int64
		Status    guber.Status
	}{
		{
			Remaining: 1,
			Status:    guber.Status_UNDER_LIMIT,
		},
		{
			Remaining: 0,
			Status:    guber.Status_UNDER_LIMIT,
		},
		{
			Remaining: 0,
			Status:    guber.Status_OVER_LIMIT,
		},
	}

	for _, test := range tests {
		resp, err := client.GetRateLimits(context.Background(), &guber.GetRateLimitsReq{
			Requests: []*guber.RateLimitReq{
				{
					Name:      "test_over_limit",
					UniqueKey: "account:1234",
					Algorithm: guber.Algorithm_TOKEN_BUCKET,
					Duration:  guber.Second * 9,
					Limit:     2,
					Hits:      1,
					Behavior:  0,
				},
			},
		})
		require.NoError(t, err)

		rl := resp.Responses[0]

		assert.Equal(t, test.Status, rl.Status)
		assert.Equal(t, test.Remaining, rl.Remaining)
		assert.Equal(t, int64(2), rl.Limit)
		assert.True(t, rl.ResetTime != 0)
	}
}

// TestMultipleAsync tests a regression that occurred when a client requests multiple
// rate limits that are asynchronously sent to other nodes.
func TestMultipleAsync(t *testing.T) {
	// If the consistent hash changes or the number of peers changes, this might
	// need to be changed. We want the test to forward both rate limits to other
	// nodes in the cluster.

	t.Logf("Asking Peer: %s", cluster.GetPeers()[0].GRPCAddress)
	client, errs := guber.DialV1Server(cluster.GetPeers()[0].GRPCAddress, nil)
	require.NoError(t, errs)

	resp, err := client.GetRateLimits(context.Background(), &guber.GetRateLimitsReq{
		Requests: []*guber.RateLimitReq{
			{
				Name:      "test_multiple_async",
				UniqueKey: "account:9234",
				Algorithm: guber.Algorithm_TOKEN_BUCKET,
				Duration:  guber.Second * 9,
				Limit:     2,
				Hits:      1,
				Behavior:  0,
			},
			{
				Name:      "test_multiple_async",
				UniqueKey: "account:5678",
				Algorithm: guber.Algorithm_TOKEN_BUCKET,
				Duration:  guber.Second * 9,
				Limit:     10,
				Hits:      5,
				Behavior:  0,
			},
		},
	})
	require.NoError(t, err)

	require.Len(t, resp.Responses, 2)

	rl := resp.Responses[0]
	assert.Equal(t, guber.Status_UNDER_LIMIT, rl.Status)
	assert.Equal(t, int64(1), rl.Remaining)
	assert.Equal(t, int64(2), rl.Limit)

	rl = resp.Responses[1]
	assert.Equal(t, guber.Status_UNDER_LIMIT, rl.Status)
	assert.Equal(t, int64(5), rl.Remaining)
	assert.Equal(t, int64(10), rl.Limit)
}

func TestTokenBucket(t *testing.T) {
	defer clock.Freeze(clock.Now()).Unfreeze()

	addr := cluster.GetRandomPeer(cluster.DataCenterNone).GRPCAddress
	client, err := guber.DialV1Server(addr, nil)
	require.NoError(t, err)

	tests := []struct {
		name      string
		Remaining int64
		Status    guber.Status
		Sleep     clock.Duration
	}{
		{
			name:      "remaining should be one",
			Remaining: 1,
			Status:    guber.Status_UNDER_LIMIT,
			Sleep:     clock.Duration(0),
		},
		{
			name:      "remaining should be zero and under limit",
			Remaining: 0,
			Status:    guber.Status_UNDER_LIMIT,
			Sleep:     clock.Millisecond * 100,
		},
		{
			name:      "after waiting 100ms remaining should be 1 and under limit",
			Remaining: 1,
			Status:    guber.Status_UNDER_LIMIT,
			Sleep:     clock.Duration(0),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp, err := client.GetRateLimits(context.Background(), &guber.GetRateLimitsReq{
				Requests: []*guber.RateLimitReq{
					{
						Name:      "test_token_bucket",
						UniqueKey: "account:1234",
						Algorithm: guber.Algorithm_TOKEN_BUCKET,
						Duration:  guber.Millisecond * 5,
						Limit:     2,
						Hits:      1,
					},
				},
			})
			require.NoError(t, err)

			rl := resp.Responses[0]

			assert.Empty(t, rl.Error)
			assert.Equal(t, tt.Status, rl.Status)
			assert.Equal(t, tt.Remaining, rl.Remaining)
			assert.Equal(t, int64(2), rl.Limit)
			assert.True(t, rl.ResetTime != 0)
			clock.Advance(tt.Sleep)
		})
	}
}

func TestTokenBucketGregorian(t *testing.T) {
	defer clock.Freeze(clock.Now()).Unfreeze()

	client, err := guber.DialV1Server(cluster.GetRandomPeer(cluster.DataCenterNone).GRPCAddress, nil)
	require.NoError(t, err)

	tests := []struct {
		Name      string
		Remaining int64
		Status    guber.Status
		Sleep     clock.Duration
		Hits      int64
	}{
		{
			Name:      "first hit",
			Hits:      1,
			Remaining: 59,
			Status:    guber.Status_UNDER_LIMIT,
		},
		{
			Name:      "second hit",
			Hits:      1,
			Remaining: 58,
			Status:    guber.Status_UNDER_LIMIT,
		},
		{
			Name:      "consume remaining hits",
			Hits:      58,
			Remaining: 0,
			Status:    guber.Status_UNDER_LIMIT,
		},
		{
			Name:      "should be over the limit",
			Hits:      1,
			Remaining: 0,
			Status:    guber.Status_OVER_LIMIT,
			Sleep:     clock.Second * 61,
		},
		{
			Name:      "should be under the limit",
			Hits:      0,
			Remaining: 60,
			Status:    guber.Status_UNDER_LIMIT,
		},
	}

	for _, test := range tests {
		t.Run(test.Name, func(t *testing.T) {
			resp, err := client.GetRateLimits(context.Background(), &guber.GetRateLimitsReq{
				Requests: []*guber.RateLimitReq{
					{
						Name:      "test_token_bucket_greg",
						UniqueKey: "account:12345",
						Behavior:  guber.Behavior_DURATION_IS_GREGORIAN,
						Algorithm: guber.Algorithm_TOKEN_BUCKET,
						Duration:  guber.GregorianMinutes,
						Hits:      test.Hits,
						Limit:     60,
					},
				},
			})
			require.NoError(t, err)

			rl := resp.Responses[0]

			assert.Empty(t, rl.Error)
			assert.Equal(t, test.Status, rl.Status)
			assert.Equal(t, test.Remaining, rl.Remaining)
			assert.Equal(t, int64(60), rl.Limit)
			assert.True(t, rl.ResetTime != 0)
			clock.Advance(test.Sleep)
		})
	}
}

func TestTokenBucketNegativeHits(t *testing.T) {
	defer clock.Freeze(clock.Now()).Unfreeze()

	addr := cluster.GetRandomPeer(cluster.DataCenterNone).GRPCAddress
	client, err := guber.DialV1Server(addr, nil)
	require.NoError(t, err)

	tests := []struct {
		name      string
		Remaining int64
		Status    guber.Status
		Sleep     clock.Duration
		Hits      int64
	}{
		{
			name:      "remaining should be three",
			Remaining: 3,
			Status:    guber.Status_UNDER_LIMIT,
			Sleep:     clock.Duration(0),
			Hits:      -1,
		},
		{
			name:      "remaining should be four and under limit",
			Remaining: 4,
			Status:    guber.Status_UNDER_LIMIT,
			Sleep:     clock.Duration(0),
			Hits:      -1,
		},
		{
			name:      "remaining should be 0 and under limit",
			Remaining: 0,
			Status:    guber.Status_UNDER_LIMIT,
			Sleep:     clock.Duration(0),
			Hits:      4,
		},
		{
			name:      "remaining should be 1 and under limit",
			Remaining: 1,
			Status:    guber.Status_UNDER_LIMIT,
			Sleep:     clock.Duration(0),
			Hits:      -1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp, err := client.GetRateLimits(context.Background(), &guber.GetRateLimitsReq{
				Requests: []*guber.RateLimitReq{
					{
						Name:      "test_token_bucket_negative",
						UniqueKey: "account:12345",
						Algorithm: guber.Algorithm_TOKEN_BUCKET,
						Duration:  guber.Millisecond * 5,
						Limit:     2,
						Hits:      tt.Hits,
					},
				},
			})
			require.NoError(t, err)

			rl := resp.Responses[0]

			assert.Empty(t, rl.Error)
			assert.Equal(t, tt.Status, rl.Status)
			assert.Equal(t, tt.Remaining, rl.Remaining)
			assert.Equal(t, int64(2), rl.Limit)
			assert.True(t, rl.ResetTime != 0)
			clock.Advance(tt.Sleep)
		})
	}
}

func TestDrainOverLimit(t *testing.T) {
	defer clock.Freeze(clock.Now()).Unfreeze()
	client, err := guber.DialV1Server(cluster.PeerAt(0).GRPCAddress, nil)
	require.NoError(t, err)

	tests := []struct {
		Name      string
		Hits      int64
		Remaining int64
		Status    guber.Status
	}{
		{
			Name:      "check remaining before hit",
			Hits:      0,
			Remaining: 10,
			Status:    guber.Status_UNDER_LIMIT,
		}, {
			Name:      "first hit",
			Hits:      1,
			Remaining: 9,
			Status:    guber.Status_UNDER_LIMIT,
		}, {
			Name:      "over limit hit",
			Hits:      100,
			Remaining: 0,
			Status:    guber.Status_OVER_LIMIT,
		}, {
			Name:      "check remaining",
			Hits:      0,
			Remaining: 0,
			Status:    guber.Status_UNDER_LIMIT,
		},
	}

	for idx, algoCase := range []guber.Algorithm{guber.Algorithm_TOKEN_BUCKET, guber.Algorithm_LEAKY_BUCKET} {
		t.Run(guber.Algorithm_name[int32(algoCase)], func(t *testing.T) {
			for _, test := range tests {
				ctx := context.Background()
				t.Run(test.Name, func(t *testing.T) {
					resp, err := client.GetRateLimits(ctx, &guber.GetRateLimitsReq{
						Requests: []*guber.RateLimitReq{
							{
								Name:      "test_drain_over_limit",
								UniqueKey: fmt.Sprintf("account:1234:%d", idx),
								Algorithm: algoCase,
								Behavior:  guber.Behavior_DRAIN_OVER_LIMIT,
								Duration:  guber.Second * 30,
								Hits:      test.Hits,
								Limit:     10,
							},
						},
					})
					require.NoError(t, err)
					require.Len(t, resp.Responses, 1)

					rl := resp.Responses[0]
					assert.Equal(t, test.Status, rl.Status)
					assert.Equal(t, test.Remaining, rl.Remaining)
					assert.Equal(t, int64(10), rl.Limit)
					assert.NotZero(t, rl.ResetTime)
				})
			}
		})
	}
}

func TestTokenBucketRequestMoreThanAvailable(t *testing.T) {
	defer clock.Freeze(clock.Now()).Unfreeze()

	client, err := guber.DialV1Server(cluster.GetRandomPeer(cluster.DataCenterNone).GRPCAddress, nil)
	require.NoError(t, err)

	sendHit := func(status guber.Status, remain int64, hit int64) *guber.RateLimitResp {
		ctx, cancel := context.WithTimeout(context.Background(), clock.Second*10)
		defer cancel()
		resp, err := client.GetRateLimits(ctx, &guber.GetRateLimitsReq{
			Requests: []*guber.RateLimitReq{
				{
					Name:      "test_token_more_than_available",
					UniqueKey: "account:123456",
					Algorithm: guber.Algorithm_TOKEN_BUCKET,
					Duration:  guber.Millisecond * 1000,
					Hits:      hit,
					Limit:     2000,
				},
			},
		})
		require.NoError(t, err, hit)
		assert.Equal(t, "", resp.Responses[0].Error)
		assert.Equal(t, status, resp.Responses[0].Status)
		assert.Equal(t, remain, resp.Responses[0].Remaining)
		assert.Equal(t, int64(2000), resp.Responses[0].Limit)
		return resp.Responses[0]
	}

	// Use half of the bucket
	sendHit(guber.Status_UNDER_LIMIT, 1000, 1000)

	// Ask for more than the bucket has and the remainder is still 1000.
	// See NOTE in algorithms.go
	sendHit(guber.Status_OVER_LIMIT, 1000, 1500)

	// Now other clients can ask for some of the remaining until we hit our limit
	sendHit(guber.Status_UNDER_LIMIT, 500, 500)
	sendHit(guber.Status_UNDER_LIMIT, 100, 400)
	sendHit(guber.Status_UNDER_LIMIT, 0, 100)
	sendHit(guber.Status_OVER_LIMIT, 0, 1)
}

func TestLeakyBucket(t *testing.T) {
	defer clock.Freeze(clock.Now()).Unfreeze()

	client, err := guber.DialV1Server(cluster.PeerAt(0).GRPCAddress, nil)
	require.NoError(t, err)

	tests := []struct {
		Name      string
		Hits      int64
		Remaining int64
		Status    guber.Status
		Sleep     clock.Duration
	}{
		{
			Name:      "first hit",
			Hits:      1,
			Remaining: 9,
			Status:    guber.Status_UNDER_LIMIT,
			Sleep:     clock.Second,
		},
		{
			Name:      "second hit; no leak",
			Hits:      1,
			Remaining: 8,
			Status:    guber.Status_UNDER_LIMIT,
			Sleep:     clock.Second,
		},
		{
			Name:      "third hit; no leak",
			Hits:      1,
			Remaining: 7,
			Status:    guber.Status_UNDER_LIMIT,
			Sleep:     clock.Millisecond * 1500,
		},
		{
			Name:      "should leak one hit 3 seconds after first hit",
			Hits:      0,
			Remaining: 8,
			Status:    guber.Status_UNDER_LIMIT,
			Sleep:     clock.Second * 3,
		},
		{
			Name:      "3 Seconds later we should have leaked another hit",
			Hits:      0,
			Remaining: 9,
			Status:    guber.Status_UNDER_LIMIT,
			Sleep:     clock.Duration(0),
		},
		{
			Name:      "max out our bucket and sleep for 3 seconds",
			Hits:      9,
			Remaining: 0,
			Status:    guber.Status_UNDER_LIMIT,
			Sleep:     clock.Duration(0),
		},
		{
			Name:      "should be over the limit",
			Hits:      1,
			Remaining: 0,
			Status:    guber.Status_OVER_LIMIT,
			Sleep:     clock.Second * 3,
		},
		{
			Name:      "should have leaked 1 hit",
			Hits:      0,
			Remaining: 1,
			Status:    guber.Status_UNDER_LIMIT,
			Sleep:     clock.Second * 60,
		},
		{
			Name:      "should max out the limit",
			Hits:      0,
			Remaining: 10,
			Status:    guber.Status_UNDER_LIMIT,
			Sleep:     clock.Second * 60,
		},
		{
			Name:      "should use up the limit and wait until 1 second before duration period",
			Hits:      10,
			Remaining: 0,
			Status:    guber.Status_UNDER_LIMIT,
			Sleep:     clock.Second * 29,
		},
		{
			Name:      "should use up all hits one second before duration period",
			Hits:      9,
			Remaining: 0,
			Status:    guber.Status_UNDER_LIMIT,
			Sleep:     clock.Second * 3,
		},
		{
			Name:      "only have 1 hit remaining",
			Hits:      1,
			Remaining: 0,
			Status:    guber.Status_UNDER_LIMIT,
			Sleep:     clock.Second,
		},
	}

	for _, test := range tests {
		t.Run(test.Name, func(t *testing.T) {
			resp, err := client.GetRateLimits(context.Background(), &guber.GetRateLimitsReq{
				Requests: []*guber.RateLimitReq{
					{
						Name:      "test_leaky_bucket",
						UniqueKey: "account:1234",
						Algorithm: guber.Algorithm_LEAKY_BUCKET,
						Duration:  guber.Second * 30,
						Hits:      test.Hits,
						Limit:     10,
					},
				},
			})
			require.NoError(t, err)
			require.Len(t, resp.Responses, 1)

			rl := resp.Responses[0]

			assert.Equal(t, test.Status, rl.Status)
			assert.Equal(t, test.Remaining, rl.Remaining)
			assert.Equal(t, int64(10), rl.Limit)
			assert.Equal(t, clock.Now().Unix()+(rl.Limit-rl.Remaining)*3, rl.ResetTime/1000)
			clock.Advance(test.Sleep)
		})
	}
}

func TestLeakyBucketWithBurst(t *testing.T) {
	defer clock.Freeze(clock.Now()).Unfreeze()

	client, err := guber.DialV1Server(cluster.PeerAt(0).GRPCAddress, nil)
	require.NoError(t, err)

	tests := []struct {
		Name      string
		Hits      int64
		Remaining int64
		Status    guber.Status
		Sleep     clock.Duration
	}{
		{
			Name:      "first hit",
			Hits:      1,
			Remaining: 19,
			Status:    guber.Status_UNDER_LIMIT,
			Sleep:     clock.Second,
		},
		{
			Name:      "second hit; no leak",
			Hits:      1,
			Remaining: 18,
			Status:    guber.Status_UNDER_LIMIT,
			Sleep:     clock.Second,
		},
		{
			Name:      "third hit; no leak",
			Hits:      1,
			Remaining: 17,
			Status:    guber.Status_UNDER_LIMIT,
			Sleep:     clock.Millisecond * 1500,
		},
		{
			Name:      "should leak one hit 3 seconds after first hit",
			Hits:      0,
			Remaining: 18,
			Status:    guber.Status_UNDER_LIMIT,
			Sleep:     clock.Second * 3,
		},
		{
			Name:      "3 Seconds later we should have leaked another hit",
			Hits:      0,
			Remaining: 19,
			Status:    guber.Status_UNDER_LIMIT,
			Sleep:     clock.Duration(0),
		},
		{
			Name:      "max out our bucket and sleep for 3 seconds",
			Hits:      19,
			Remaining: 0,
			Status:    guber.Status_UNDER_LIMIT,
			Sleep:     clock.Duration(0),
		},
		{
			Name:      "should be over the limit",
			Hits:      1,
			Remaining: 0,
			Status:    guber.Status_OVER_LIMIT,
			Sleep:     clock.Second * 3,
		},
		{
			Name:      "should have leaked 1 hit",
			Hits:      0,
			Remaining: 1,
			Status:    guber.Status_UNDER_LIMIT,
			Sleep:     clock.Second * 60,
		},
		{
			Name:      "should max out remaining",
			Hits:      0,
			Remaining: 20,
			Status:    guber.Status_UNDER_LIMIT,
			Sleep:     clock.Second,
		},
	}

	for _, test := range tests {
		t.Run(test.Name, func(t *testing.T) {
			resp, err := client.GetRateLimits(context.Background(), &guber.GetRateLimitsReq{
				Requests: []*guber.RateLimitReq{
					{
						Name:      "test_leaky_bucket_with_burst",
						UniqueKey: "account:1234",
						Algorithm: guber.Algorithm_LEAKY_BUCKET,
						Duration:  guber.Second * 30,
						Hits:      test.Hits,
						Limit:     10,
						Burst:     20,
					},
				},
			})
			require.NoError(t, err)
			require.Len(t, resp.Responses, 1)

			rl := resp.Responses[0]

			assert.Equal(t, test.Status, rl.Status)
			assert.Equal(t, test.Remaining, rl.Remaining)
			assert.Equal(t, int64(10), rl.Limit)
			assert.Equal(t, clock.Now().Unix()+(rl.Limit-rl.Remaining)*3, rl.ResetTime/1000)
			clock.Advance(test.Sleep)
		})
	}
}

func TestLeakyBucketGregorian(t *testing.T) {
	defer clock.Freeze(clock.Now()).Unfreeze()

	client, err := guber.DialV1Server(cluster.PeerAt(0).GRPCAddress, nil)
	require.NoError(t, err)

	tests := []struct {
		Name      string
		Hits      int64
		Remaining int64
		Status    guber.Status
		Sleep     clock.Duration
	}{
		{
			Name:      "first hit",
			Hits:      1,
			Remaining: 59,
			Status:    guber.Status_UNDER_LIMIT,
			Sleep:     clock.Millisecond * 500,
		},
		{
			Name:      "second hit; no leak",
			Hits:      1,
			Remaining: 58,
			Status:    guber.Status_UNDER_LIMIT,
			Sleep:     clock.Millisecond * 1200,
		},
		{
			Name:      "third hit; leak one hit",
			Hits:      1,
			Remaining: 58,
			Status:    guber.Status_UNDER_LIMIT,
		},
	}

	// Truncate to the nearest minute.
	now := clock.Now()
	trunc := now.Truncate(time.Hour)
	trunc = now.Add(now.Sub(trunc))
	clock.Advance(now.Sub(trunc))

	name := t.Name()
	key := guber.RandomString(10)

	for _, test := range tests {
		t.Run(test.Name, func(t *testing.T) {
			resp, err := client.GetRateLimits(context.Background(), &guber.GetRateLimitsReq{
				Requests: []*guber.RateLimitReq{
					{
						Name:      name,
						UniqueKey: key,
						Behavior:  guber.Behavior_DURATION_IS_GREGORIAN,
						Algorithm: guber.Algorithm_LEAKY_BUCKET,
						Duration:  guber.GregorianMinutes,
						Hits:      test.Hits,
						Limit:     60,
					},
				},
			})
			require.NoError(t, err)

			rl := resp.Responses[0]
			assert.Equal(t, test.Status, rl.Status)
			assert.Equal(t, test.Remaining, rl.Remaining)
			assert.Equal(t, int64(60), rl.Limit)
			assert.Greater(t, rl.ResetTime, now.Unix())
			clock.Advance(test.Sleep)
		})
	}
}

func TestLeakyBucketNegativeHits(t *testing.T) {
	defer clock.Freeze(clock.Now()).Unfreeze()

	client, err := guber.DialV1Server(cluster.PeerAt(0).GRPCAddress, nil)
	require.NoError(t, err)

	tests := []struct {
		Name      string
		Hits      int64
		Remaining int64
		Status    guber.Status
		Sleep     clock.Duration
	}{
		{
			Name:      "first hit",
			Hits:      1,
			Remaining: 9,
			Status:    guber.Status_UNDER_LIMIT,
			Sleep:     clock.Duration(0),
		},
		{
			Name:      "can increase remaining",
			Hits:      -1,
			Remaining: 10,
			Status:    guber.Status_UNDER_LIMIT,
			Sleep:     clock.Duration(0),
		},
		{
			Name:      "remaining should be zero",
			Hits:      10,
			Remaining: 0,
			Status:    guber.Status_UNDER_LIMIT,
			Sleep:     clock.Duration(0),
		},
		{
			Name:      "can append one to remaining when remaining is zero",
			Hits:      -1,
			Remaining: 1,
			Status:    guber.Status_UNDER_LIMIT,
			Sleep:     clock.Duration(0),
		},
	}

	for _, test := range tests {
		t.Run(test.Name, func(t *testing.T) {
			resp, err := client.GetRateLimits(context.Background(), &guber.GetRateLimitsReq{
				Requests: []*guber.RateLimitReq{
					{
						Name:      "test_leaky_bucket_negative",
						UniqueKey: "account:12345",
						Algorithm: guber.Algorithm_LEAKY_BUCKET,
						Duration:  guber.Second * 30,
						Hits:      test.Hits,
						Limit:     10,
					},
				},
			})
			require.NoError(t, err)
			require.Len(t, resp.Responses, 1)

			rl := resp.Responses[0]

			assert.Equal(t, test.Status, rl.Status)
			assert.Equal(t, test.Remaining, rl.Remaining)
			assert.Equal(t, int64(10), rl.Limit)
			assert.Equal(t, clock.Now().Unix()+(rl.Limit-rl.Remaining)*3, rl.ResetTime/1000)
			clock.Advance(test.Sleep)
		})
	}
}

func TestLeakyBucketRequestMoreThanAvailable(t *testing.T) {
	// Freeze time so we don't leak during the test
	defer clock.Freeze(clock.Now()).Unfreeze()

	client, err := guber.DialV1Server(cluster.GetRandomPeer(cluster.DataCenterNone).GRPCAddress, nil)
	require.NoError(t, err)

	sendHit := func(status guber.Status, remain int64, hits int64) *guber.RateLimitResp {
		ctx, cancel := context.WithTimeout(context.Background(), clock.Second*10)
		defer cancel()
		resp, err := client.GetRateLimits(ctx, &guber.GetRateLimitsReq{
			Requests: []*guber.RateLimitReq{
				{
					Name:      "test_leaky_more_than_available",
					UniqueKey: "account:123456",
					Algorithm: guber.Algorithm_LEAKY_BUCKET,
					Duration:  guber.Millisecond * 1000,
					Hits:      hits,
					Limit:     2000,
				},
			},
		})
		require.NoError(t, err)
		assert.Equal(t, "", resp.Responses[0].Error)
		assert.Equal(t, status, resp.Responses[0].Status)
		assert.Equal(t, remain, resp.Responses[0].Remaining)
		assert.Equal(t, int64(2000), resp.Responses[0].Limit)
		return resp.Responses[0]
	}

	// Use half of the bucket
	sendHit(guber.Status_UNDER_LIMIT, 1000, 1000)

	// Ask for more than the rate limit has and the remainder is still 1000.
	// See NOTE in algorithms.go
	sendHit(guber.Status_OVER_LIMIT, 1000, 1500)

	// Now other clients can ask for some of the remaining until we hit our limit
	sendHit(guber.Status_UNDER_LIMIT, 500, 500)
	sendHit(guber.Status_UNDER_LIMIT, 100, 400)
	sendHit(guber.Status_UNDER_LIMIT, 0, 100)
	sendHit(guber.Status_OVER_LIMIT, 0, 1)
}

func TestMissingFields(t *testing.T) {
	client, err := guber.DialV1Server(cluster.GetRandomPeer(cluster.DataCenterNone).GRPCAddress, nil)
	require.NoError(t, err)

	tests := []struct {
		Req    *guber.RateLimitReq
		Status guber.Status
		Error  string
	}{
		{
			Req: &guber.RateLimitReq{
				Name:      "test_missing_fields",
				UniqueKey: "account:1234",
				Hits:      1,
				Limit:     10,
				Duration:  0,
			},
			Error:  "", // No Error
			Status: guber.Status_UNDER_LIMIT,
		},
		{
			Req: &guber.RateLimitReq{
				Name:      "test_missing_fields",
				UniqueKey: "account:12345",
				Hits:      1,
				Duration:  10000,
				Limit:     0,
			},
			Error:  "", // No Error
			Status: guber.Status_OVER_LIMIT,
		},
		{
			Req: &guber.RateLimitReq{
				UniqueKey: "account:1234",
				Hits:      1,
				Duration:  10000,
				Limit:     5,
			},
			Error:  "field 'namespace' cannot be empty",
			Status: guber.Status_UNDER_LIMIT,
		},
		{
			Req: &guber.RateLimitReq{
				Name:     "test_missing_fields",
				Hits:     1,
				Duration: 10000,
				Limit:    5,
			},
			Error:  "field 'unique_key' cannot be empty",
			Status: guber.Status_UNDER_LIMIT,
		},
	}

	for i, test := range tests {
		resp, err := client.GetRateLimits(context.Background(), &guber.GetRateLimitsReq{
			Requests: []*guber.RateLimitReq{test.Req},
		})
		require.NoError(t, err)
		assert.Equal(t, test.Error, resp.Responses[0].Error, i)
		assert.Equal(t, test.Status, resp.Responses[0].Status, i)
	}
}

func TestGlobalRateLimits(t *testing.T) {
	name := t.Name()
	key := guber.RandomString(10)
	owner, err := cluster.FindOwningDaemon(name, key)
	require.NoError(t, err)
	peers, err := cluster.ListNonOwningDaemons(name, key)
	require.NoError(t, err)
	var firstResetTime int64

	sendHit := func(client guber.V1Client, status guber.Status, hits, remain int64) {
		ctx, cancel := context.WithTimeout(context.Background(), clock.Second*10)
		defer cancel()
		resp, err := client.GetRateLimits(ctx, &guber.GetRateLimitsReq{
			Requests: []*guber.RateLimitReq{
				{
					Name:      name,
					UniqueKey: key,
					Algorithm: guber.Algorithm_TOKEN_BUCKET,
					Behavior:  guber.Behavior_GLOBAL,
					Duration:  guber.Minute * 3,
					Hits:      hits,
					Limit:     5,
				},
			},
		})
		require.NoError(t, err)
		item := resp.Responses[0]
		assert.Equal(t, "", item.Error)
		assert.Equal(t, remain, item.Remaining)
		assert.Equal(t, status, item.Status)
		assert.Equal(t, int64(5), item.Limit)

		// ResetTime should not change during test.
		if firstResetTime == 0 {
			firstResetTime = item.ResetTime
		}
		assert.Equal(t, firstResetTime, item.ResetTime)

		// ensure that we have a canonical host
		assert.NotEmpty(t, item.Metadata["owner"])
	}

	require.NoError(t, waitForIdle(1*clock.Minute, cluster.GetDaemons()...))

	// Our first hit should create the request on the peer and queue for async forward
	sendHit(peers[0].MustClient(), guber.Status_UNDER_LIMIT, 1, 4)

	// Our second should be processed as if we own it since the async forward hasn't occurred yet
	sendHit(peers[0].MustClient(), guber.Status_UNDER_LIMIT, 2, 2)

	testutil.UntilPass(t, 20, clock.Millisecond*200, func(t testutil.TestingT) {
		// Inspect peers metrics, ensure the peer sent the global rate limit to the owner
		metricsURL := fmt.Sprintf("http://%s/metrics", peers[0].Config().HTTPListenAddress)
		m, err := getMetricRequest(metricsURL, "gubernator_global_send_duration_count")
		assert.NoError(t, err)
		assert.Equal(t, 1, int(m.Value))
	})

	require.NoError(t, waitForBroadcast(clock.Second*3, owner, 1))

	// Check different peers, they should have gotten the broadcast from the owner
	sendHit(peers[1].MustClient(), guber.Status_UNDER_LIMIT, 0, 2)
	sendHit(peers[2].MustClient(), guber.Status_UNDER_LIMIT, 0, 2)

	// Non owning peer should calculate the rate limit remaining before forwarding
	// to the owner.
	sendHit(peers[3].MustClient(), guber.Status_UNDER_LIMIT, 2, 0)

	require.NoError(t, waitForBroadcast(clock.Second*3, owner, 2))

	sendHit(peers[4].MustClient(), guber.Status_OVER_LIMIT, 1, 0)
}

// Ensure global broadcast updates all peers when GetRateLimits is called on
// either owner or non-owner peer.
func TestGlobalRateLimitsWithLoadBalancing(t *testing.T) {
	name := t.Name()
	key := guber.RandomString(10)

	// Determine owner and non-owner peers.
	owner, err := cluster.FindOwningDaemon(name, key)
	require.NoError(t, err)
	peers, err := cluster.ListNonOwningDaemons(name, key)
	require.NoError(t, err)
	nonOwner := peers[0]

	// Connect to owner and non-owner peers in round robin.
	dialOpts := []grpc.DialOption{
		grpc.WithResolvers(guber.NewStaticBuilder()),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultServiceConfig(`{"loadBalancingConfig": [{"round_robin":{}}]}`),
	}
	address := fmt.Sprintf("static:///%s,%s", owner.PeerInfo.GRPCAddress, nonOwner.PeerInfo.GRPCAddress)
	conn, err := grpc.NewClient(address, dialOpts...)
	require.NoError(t, err)
	client := guber.NewV1Client(conn)

	sendHit := func(client guber.V1Client, status guber.Status, i int) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*clock.Second)
		defer cancel()
		resp, err := client.GetRateLimits(ctx, &guber.GetRateLimitsReq{
			Requests: []*guber.RateLimitReq{
				{
					Name:      name,
					UniqueKey: key,
					Algorithm: guber.Algorithm_TOKEN_BUCKET,
					Behavior:  guber.Behavior_GLOBAL,
					Duration:  5 * guber.Minute,
					Hits:      1,
					Limit:     2,
				},
			},
		})
		require.NoError(t, err, i)
		item := resp.Responses[0]
		assert.Equal(t, "", item.Error, fmt.Sprintf("unexpected error, iteration %d", i))
		assert.Equal(t, status, item.Status, fmt.Sprintf("mismatch status, iteration %d", i))
	}

	require.NoError(t, waitForIdle(1*clock.Minute, cluster.GetDaemons()...))

	// Send two hits that should be processed by the owner and non-owner and
	// deplete the limit consistently.
	sendHit(client, guber.Status_UNDER_LIMIT, 1)
	sendHit(client, guber.Status_UNDER_LIMIT, 2)
	require.NoError(t, waitForBroadcast(3*clock.Second, owner, 1))

	// All successive hits should return OVER_LIMIT.
	for i := 2; i <= 10; i++ {
		sendHit(client, guber.Status_OVER_LIMIT, i)
	}
}

func TestGlobalRateLimitsPeerOverLimit(t *testing.T) {
	name := t.Name()
	key := guber.RandomString(10)
	owner, err := cluster.FindOwningDaemon(name, key)
	require.NoError(t, err)
	peers, err := cluster.ListNonOwningDaemons(name, key)
	require.NoError(t, err)

	sendHit := func(expectedStatus guber.Status, hits, expectedRemaining int64) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*clock.Second)
		defer cancel()
		resp, err := peers[0].MustClient().GetRateLimits(ctx, &guber.GetRateLimitsReq{
			Requests: []*guber.RateLimitReq{
				{
					Name:      name,
					UniqueKey: key,
					Algorithm: guber.Algorithm_TOKEN_BUCKET,
					Behavior:  guber.Behavior_GLOBAL,
					Duration:  5 * guber.Minute,
					Hits:      hits,
					Limit:     2,
				},
			},
		})
		assert.NoError(t, err)
		item := resp.Responses[0]
		assert.Equal(t, "", item.Error, "unexpected error")
		assert.Equal(t, expectedStatus, item.Status, "mismatch status")
		assert.Equal(t, expectedRemaining, item.Remaining, "mismatch remaining")
	}

	require.NoError(t, waitForIdle(1*clock.Minute, cluster.GetDaemons()...))

	// Send two hits that should be processed by the owner and the broadcast to
	// peer, depleting the remaining.
	sendHit(guber.Status_UNDER_LIMIT, 1, 1)
	sendHit(guber.Status_UNDER_LIMIT, 1, 0)

	// Wait for the broadcast from the owner to the peer
	require.NoError(t, waitForBroadcast(3*clock.Second, owner, 1))

	// Since the remainder is 0, the peer should return OVER_LIMIT on next hit.
	sendHit(guber.Status_OVER_LIMIT, 1, 0)

	// Wait for the broadcast from the owner to the peer.
	require.NoError(t, waitForBroadcast(3*clock.Second, owner, 2))

	// The status should still be OVER_LIMIT.
	sendHit(guber.Status_OVER_LIMIT, 0, 0)
}

func TestGlobalRequestMoreThanAvailable(t *testing.T) {
	name := t.Name()
	key := guber.RandomString(10)
	owner, err := cluster.FindOwningDaemon(name, key)
	require.NoError(t, err)
	peers, err := cluster.ListNonOwningDaemons(name, key)
	require.NoError(t, err)

	sendHit := func(client guber.V1Client, expectedStatus guber.Status, hits int64, remaining int64) {
		ctx, cancel := context.WithTimeout(context.Background(), clock.Second*10)
		defer cancel()
		resp, err := client.GetRateLimits(ctx, &guber.GetRateLimitsReq{
			Requests: []*guber.RateLimitReq{
				{
					Name:      name,
					UniqueKey: key,
					Algorithm: guber.Algorithm_LEAKY_BUCKET,
					Behavior:  guber.Behavior_GLOBAL,
					Duration:  guber.Minute * 1_000,
					Hits:      hits,
					Limit:     100,
				},
			},
		})
		assert.NoError(t, err)
		assert.Equal(t, "", resp.Responses[0].GetError())
		assert.Equal(t, expectedStatus, resp.Responses[0].GetStatus())
	}

	require.NoError(t, waitForIdle(1*time.Minute, cluster.GetDaemons()...))
	prev := getMetricValue(t, owner, "gubernator_broadcast_duration_count")

	// Ensure GRPC has connections to each peer before we start, as we want
	// the actual test requests to happen quite fast.
	for _, p := range peers {
		sendHit(p.MustClient(), guber.Status_UNDER_LIMIT, 0, 100)
	}

	// Send a request for 50 hits from each non owning peer in the cluster. These requests
	// will be queued and sent to the owner as accumulated hits. As a result of the async nature
	// of `Behavior_GLOBAL` rate limit requests spread across peers like this will be allowed to
	// over-consume their resource within the rate limit window until the owner is updated and
	// a broadcast to all peers is received.
	//
	// The maximum number of resources that can be over-consumed can be calculated by multiplying
	// the remainder by the number of peers in the cluster. For example: If you have a remainder of 100
	// and a cluster of 10 instances, then the maximum over-consumed resource is 1,000. If you need
	// a more accurate remaining calculation, and wish to avoid over consuming a resource, then do
	// not use `Behavior_GLOBAL`.
	for _, p := range peers {
		sendHit(p.MustClient(), guber.Status_UNDER_LIMIT, 50, 50)
	}

	// Wait for the broadcast from the owner to the peers
	require.NoError(t, waitForBroadcast(clock.Second*10, owner, prev+1))

	// We should be over the limit
	sendHit(peers[0].MustClient(), guber.Status_OVER_LIMIT, 1, 0)
}

func TestGlobalNegativeHits(t *testing.T) {
	name := t.Name()
	key := guber.RandomString(10)
	owner, err := cluster.FindOwningDaemon(name, key)
	require.NoError(t, err)
	peers, err := cluster.ListNonOwningDaemons(name, key)
	require.NoError(t, err)

	sendHit := func(client guber.V1Client, status guber.Status, hits int64, remaining int64) {
		ctx, cancel := context.WithTimeout(context.Background(), clock.Second*10)
		defer cancel()
		resp, err := client.GetRateLimits(ctx, &guber.GetRateLimitsReq{
			Requests: []*guber.RateLimitReq{
				{
					Name:      name,
					UniqueKey: key,
					Algorithm: guber.Algorithm_TOKEN_BUCKET,
					Behavior:  guber.Behavior_GLOBAL,
					Duration:  guber.Minute * 100,
					Hits:      hits,
					Limit:     2,
				},
			},
		})
		assert.NoError(t, err)
		assert.Equal(t, "", resp.Responses[0].GetError())
		assert.Equal(t, status, resp.Responses[0].GetStatus())
		assert.Equal(t, remaining, resp.Responses[0].Remaining)
	}

	require.NoError(t, waitForIdle(1*time.Minute, cluster.GetDaemons()...))

	prev := getMetricValue(t, owner, "gubernator_broadcast_duration_count")
	require.NoError(t, err)

	// Send a negative hit on a rate limit with no hits
	sendHit(peers[0].MustClient(), guber.Status_UNDER_LIMIT, -1, 3)

	// Wait for the negative remaining to propagate
	require.NoError(t, waitForBroadcast(clock.Second*10, owner, prev+1))

	// Send another negative hit to a different peer
	sendHit(peers[1].MustClient(), guber.Status_UNDER_LIMIT, -1, 4)

	require.NoError(t, waitForBroadcast(clock.Second*10, owner, prev+2))

	// Should have 4 in the remainder
	sendHit(peers[2].MustClient(), guber.Status_UNDER_LIMIT, 4, 0)

	require.NoError(t, waitForBroadcast(clock.Second*10, owner, prev+3))

	sendHit(peers[3].MustClient(), guber.Status_UNDER_LIMIT, 0, 0)
}

func TestGlobalResetRemaining(t *testing.T) {
	name := t.Name()
	key := guber.RandomString(10)
	owner, err := cluster.FindOwningDaemon(name, key)
	require.NoError(t, err)
	peers, err := cluster.ListNonOwningDaemons(name, key)
	require.NoError(t, err)

	sendHit := func(client guber.V1Client, expectedStatus guber.Status, hits int64, remaining int64) {
		ctx, cancel := context.WithTimeout(context.Background(), clock.Second*10)
		defer cancel()
		resp, err := client.GetRateLimits(ctx, &guber.GetRateLimitsReq{
			Requests: []*guber.RateLimitReq{
				{
					Name:      name,
					UniqueKey: key,
					Algorithm: guber.Algorithm_LEAKY_BUCKET,
					Behavior:  guber.Behavior_GLOBAL,
					Duration:  guber.Minute * 1_000,
					Hits:      hits,
					Limit:     100,
				},
			},
		})
		assert.NoError(t, err)
		assert.Equal(t, "", resp.Responses[0].GetError())
		assert.Equal(t, expectedStatus, resp.Responses[0].GetStatus())
		assert.Equal(t, remaining, resp.Responses[0].Remaining)
	}

	require.NoError(t, waitForIdle(1*time.Minute, cluster.GetDaemons()...))

	prev := getMetricValue(t, owner, "gubernator_broadcast_duration_count")
	require.NoError(t, err)

	for _, p := range peers {
		sendHit(p.MustClient(), guber.Status_UNDER_LIMIT, 50, 50)
	}

	// Wait for the broadcast from the owner to the peers
	require.NoError(t, waitForBroadcast(clock.Second*10, owner, prev+1))

	// We should be over the limit and remaining should be zero
	sendHit(peers[0].MustClient(), guber.Status_OVER_LIMIT, 1, 0)

	// Now reset the remaining
	ctx, cancel := context.WithTimeout(context.Background(), clock.Second*10)
	defer cancel()
	resp, err := peers[0].MustClient().GetRateLimits(ctx, &guber.GetRateLimitsReq{
		Requests: []*guber.RateLimitReq{
			{
				Name:      name,
				UniqueKey: key,
				Algorithm: guber.Algorithm_LEAKY_BUCKET,
				Behavior:  guber.Behavior_GLOBAL | guber.Behavior_RESET_REMAINING,
				Duration:  guber.Minute * 1_000,
				Hits:      0,
				Limit:     100,
			},
		},
	})
	require.NoError(t, err)
	assert.NotEqual(t, 100, resp.Responses[0].Remaining)

	// Wait for the reset to propagate.
	require.NoError(t, waitForBroadcast(clock.Second*10, owner, prev+2))

	// Check a different peer to ensure remaining has been reset
	resp, err = peers[1].MustClient().GetRateLimits(ctx, &guber.GetRateLimitsReq{
		Requests: []*guber.RateLimitReq{
			{
				Name:      name,
				UniqueKey: key,
				Algorithm: guber.Algorithm_LEAKY_BUCKET,
				Behavior:  guber.Behavior_GLOBAL,
				Duration:  guber.Minute * 1_000,
				Hits:      0,
				Limit:     100,
			},
		},
	})
	require.NoError(t, err)
	assert.NotEqual(t, 100, resp.Responses[0].Remaining)
}

func TestChangeLimit(t *testing.T) {
	client, err := guber.DialV1Server(cluster.GetRandomPeer(cluster.DataCenterNone).GRPCAddress, nil)
	require.NoError(t, err)

	tests := []struct {
		Remaining int64
		Algorithm guber.Algorithm
		Status    guber.Status
		Name      string
		Limit     int64
	}{
		{
			Name:      "Should subtract 1 from remaining",
			Algorithm: guber.Algorithm_TOKEN_BUCKET,
			Status:    guber.Status_UNDER_LIMIT,
			Remaining: 99,
			Limit:     100,
		},
		{
			Name:      "Should subtract 1 from remaining",
			Algorithm: guber.Algorithm_TOKEN_BUCKET,
			Status:    guber.Status_UNDER_LIMIT,
			Remaining: 98,
			Limit:     100,
		},
		{
			Name:      "Should subtract 1 from remaining and change limit to 10",
			Algorithm: guber.Algorithm_TOKEN_BUCKET,
			Status:    guber.Status_UNDER_LIMIT,
			Remaining: 7,
			Limit:     10,
		},
		{
			Name:      "Should subtract 1 from remaining with new limit of 10",
			Algorithm: guber.Algorithm_TOKEN_BUCKET,
			Status:    guber.Status_UNDER_LIMIT,
			Remaining: 6,
			Limit:     10,
		},
		{
			Name:      "Should subtract 1 from remaining with new limit of 200",
			Algorithm: guber.Algorithm_TOKEN_BUCKET,
			Status:    guber.Status_UNDER_LIMIT,
			Remaining: 195,
			Limit:     200,
		},
		{
			Name:      "Should subtract 1 from remaining for leaky bucket",
			Algorithm: guber.Algorithm_LEAKY_BUCKET,
			Status:    guber.Status_UNDER_LIMIT,
			Remaining: 99,
			Limit:     100,
		},
		{
			Name:      "Should subtract 1 from remaining for leaky bucket after limit change",
			Algorithm: guber.Algorithm_LEAKY_BUCKET,
			Status:    guber.Status_UNDER_LIMIT,
			Remaining: 9,
			Limit:     10,
		},
		{
			Name:      "Should subtract 1 from remaining for leaky bucket with new limit",
			Algorithm: guber.Algorithm_LEAKY_BUCKET,
			Status:    guber.Status_UNDER_LIMIT,
			Remaining: 8,
			Limit:     10,
		},
	}

	for _, tt := range tests {
		t.Run(tt.Name, func(t *testing.T) {
			resp, err := client.GetRateLimits(context.Background(), &guber.GetRateLimitsReq{
				Requests: []*guber.RateLimitReq{
					{
						Name:      "test_change_limit",
						UniqueKey: "account:1234",
						Algorithm: tt.Algorithm,
						Duration:  guber.Millisecond * 9000,
						Limit:     tt.Limit,
						Hits:      1,
					},
				},
			})
			require.NoError(t, err)

			rl := resp.Responses[0]

			assert.Equal(t, tt.Status, rl.Status)
			assert.Equal(t, tt.Remaining, rl.Remaining)
			assert.Equal(t, tt.Limit, rl.Limit)
			assert.True(t, rl.ResetTime != 0)
		})
	}
}

func TestResetRemaining(t *testing.T) {
	client, err := guber.DialV1Server(cluster.GetRandomPeer(cluster.DataCenterNone).GRPCAddress, nil)
	require.NoError(t, err)

	tests := []struct {
		Remaining int64
		Algorithm guber.Algorithm
		Behavior  guber.Behavior
		Status    guber.Status
		Name      string
		Limit     int64
	}{
		{
			Name:      "Should subtract 1 from remaining",
			Algorithm: guber.Algorithm_TOKEN_BUCKET,
			Status:    guber.Status_UNDER_LIMIT,
			Behavior:  guber.Behavior_BATCHING,
			Remaining: 99,
			Limit:     100,
		},
		{
			Name:      "Should subtract 2 from remaining",
			Algorithm: guber.Algorithm_TOKEN_BUCKET,
			Status:    guber.Status_UNDER_LIMIT,
			Behavior:  guber.Behavior_BATCHING,
			Remaining: 98,
			Limit:     100,
		},
		{
			Name:      "Should reset the remaining",
			Algorithm: guber.Algorithm_TOKEN_BUCKET,
			Status:    guber.Status_UNDER_LIMIT,
			Behavior:  guber.Behavior_RESET_REMAINING,
			Remaining: 100,
			Limit:     100,
		},
		{
			Name:      "Should subtract 1 from remaining after reset",
			Algorithm: guber.Algorithm_TOKEN_BUCKET,
			Status:    guber.Status_UNDER_LIMIT,
			Behavior:  guber.Behavior_BATCHING,
			Remaining: 99,
			Limit:     100,
		},
	}

	for _, tt := range tests {
		t.Run(tt.Name, func(t *testing.T) {
			resp, err := client.GetRateLimits(context.Background(), &guber.GetRateLimitsReq{
				Requests: []*guber.RateLimitReq{
					{
						Name:      "test_reset_remaining",
						UniqueKey: "account:1234",
						Algorithm: tt.Algorithm,
						Duration:  guber.Millisecond * 9000,
						Behavior:  tt.Behavior,
						Limit:     tt.Limit,
						Hits:      1,
					},
				},
			})
			require.NoError(t, err)

			rl := resp.Responses[0]

			assert.Equal(t, tt.Status, rl.Status)
			assert.Equal(t, tt.Remaining, rl.Remaining)
			assert.Equal(t, tt.Limit, rl.Limit)
		})
	}
}

func TestHealthCheck(t *testing.T) {
	// Check that the cluster is healthy to start with.
	for _, peer := range cluster.GetDaemons() {
		healthResp, err := peer.MustClient().HealthCheck(context.Background(), &guber.HealthCheckReq{})
		require.NoError(t, err)
		assert.Equal(t, "healthy", healthResp.Status)
	}

	// Stop the cluster to ensure errors occur on our instance.
	cluster.Stop()

	// Check the health again to get back the connection error.
	testutil.UntilPass(t, 20, 300*clock.Millisecond, func(t testutil.TestingT) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		for _, peer := range cluster.GetDaemons() {
			_, err := peer.MustClient().HealthCheck(ctx, &guber.HealthCheckReq{})
			assert.Error(t, err, "connect: connection refused")
		}
	})

	// Restart so cluster is ready for next test.
	require.NoError(t, startGubernator())
}

func TestLeakyBucketDivBug(t *testing.T) {
	defer clock.Freeze(clock.Now()).Unfreeze()
	name := t.Name()
	key := guber.RandomString(10)
	client, err := guber.DialV1Server(cluster.GetRandomPeer(cluster.DataCenterNone).GRPCAddress, nil)
	require.NoError(t, err)

	resp, err := client.GetRateLimits(context.Background(), &guber.GetRateLimitsReq{
		Requests: []*guber.RateLimitReq{
			{
				Name:      name,
				UniqueKey: key,
				Algorithm: guber.Algorithm_LEAKY_BUCKET,
				Duration:  guber.Millisecond * 1000,
				Hits:      1,
				Limit:     2000,
			},
		},
	})
	require.NoError(t, err)
	assert.Equal(t, "", resp.Responses[0].Error)
	assert.Equal(t, guber.Status_UNDER_LIMIT, resp.Responses[0].Status)
	assert.Equal(t, int64(1999), resp.Responses[0].Remaining)
	assert.Equal(t, int64(2000), resp.Responses[0].Limit)

	// Should result in a rate of 0.5
	resp, err = client.GetRateLimits(context.Background(), &guber.GetRateLimitsReq{
		Requests: []*guber.RateLimitReq{
			{
				Name:      name,
				UniqueKey: key,
				Algorithm: guber.Algorithm_LEAKY_BUCKET,
				Duration:  guber.Millisecond * 1000,
				Hits:      100,
				Limit:     2000,
			},
		},
	})
	require.NoError(t, err)
	assert.Equal(t, int64(1899), resp.Responses[0].Remaining)
	assert.Equal(t, int64(2000), resp.Responses[0].Limit)
}

func TestMultiRegion(t *testing.T) {

	// TODO: Queue a rate limit with multi region behavior on the DataCenterNone cluster
	// TODO: Check the immediate response is correct
	// TODO: Wait until the rate limit count shows up on the DataCenterOne and DataCenterTwo cluster

	// TODO: Increment the counts on the DataCenterTwo and DataCenterOne clusters
	// TODO: Wait until both rate limit count show up on all datacenters
}

func TestGRPCGateway(t *testing.T) {
	name := t.Name()
	key := guber.RandomString(10)
	address := cluster.GetRandomPeer(cluster.DataCenterNone).HTTPAddress
	resp, err := http.DefaultClient.Get("http://" + address + "/v1/HealthCheck")
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	b, err := io.ReadAll(resp.Body)

	// This test ensures future upgrades don't accidentally change `under_score` to `camelCase` again.
	assert.Contains(t, string(b), "peer_count")

	var hc guber.HealthCheckResp
	require.NoError(t, json.Unmarshal(b, &hc))
	assert.Equal(t, int32(10), hc.PeerCount)

	require.NoError(t, err)

	payload, err := json.Marshal(&guber.GetRateLimitsReq{
		Requests: []*guber.RateLimitReq{
			{
				Name:      name,
				UniqueKey: key,
				Duration:  guber.Millisecond * 1000,
				Hits:      1,
				Limit:     10,
			},
		},
	})
	require.NoError(t, err)

	resp, err = http.DefaultClient.Post("http://"+address+"/v1/GetRateLimits",
		"application/json", bytes.NewReader(payload))
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	b, err = io.ReadAll(resp.Body)
	require.NoError(t, err)
	var r guber.GetRateLimitsResp

	// NOTE: It is important to use 'protojson' instead of the standard 'json' package
	//  else the enums will not be converted properly and json.Unmarshal() will return an
	//  error.
	require.NoError(t, json.Unmarshal(b, &r))
	require.Equal(t, 1, len(r.Responses))
	assert.Equal(t, guber.Status_UNDER_LIMIT, r.Responses[0].Status)
}

func TestGetPeerRateLimits(t *testing.T) {
	name := t.Name()
	ctx := context.Background()
	peerClient, err := guber.NewPeerClient(guber.PeerConfig{
		Info: cluster.GetRandomPeer(cluster.DataCenterNone),
	})
	require.NoError(t, err)

	t.Run("Stable rate check request order", func(t *testing.T) {
		// Ensure response order matches rate check request order.
		// Try various batch sizes.
		createdAt := epochMillis(clock.Now())
		testCases := []int{1, 2, 5, 10, 100, 1000}

		for _, n := range testCases {
			t.Run(fmt.Sprintf("Batch size %d", n), func(t *testing.T) {
				// Build request.
				req := &guber.GetPeerRateLimitsReq{
					Requests: make([]*guber.RateLimitReq, n),
				}
				for i := 0; i < n; i++ {
					req.Requests[i] = &guber.RateLimitReq{
						Name:      name,
						UniqueKey: guber.RandomString(10),
						Hits:      0,
						Limit:     1000 + int64(i),
						Duration:  1000,
						Algorithm: guber.Algorithm_TOKEN_BUCKET,
						Behavior:  guber.Behavior_BATCHING,
						CreatedAt: &createdAt,
					}
				}

				// Send request.
				resp, err := peerClient.GetPeerRateLimits(ctx, req)

				// Verify.
				require.NoError(t, err)
				require.NotNil(t, resp)
				assert.Len(t, resp.RateLimits, n)

				for i, item := range resp.RateLimits {
					// Identify response by its unique limit.
					assert.Equal(t, req.Requests[i].Limit, item.Limit)
				}
			})
		}
	})
}

// TODO: Add a test for sending no rate limits RateLimitReqList.RateLimits = nil

func TestGlobalBehavior(t *testing.T) {
	const limit = 1000
	broadcastTimeout := 400 * time.Millisecond
	createdAt := epochMillis(clock.Now())

	makeReq := func(name, key string, hits int64) *guber.RateLimitReq {
		return &guber.RateLimitReq{
			Name:      name,
			UniqueKey: key,
			Algorithm: guber.Algorithm_TOKEN_BUCKET,
			Behavior:  guber.Behavior_GLOBAL,
			Duration:  guber.Minute * 3,
			Hits:      hits,
			Limit:     limit,
			CreatedAt: &createdAt,
		}
	}

	t.Run("Hits on owner peer", func(t *testing.T) {
		testCases := []struct {
			Name string
			Hits int64
		}{
			{Name: "Single hit", Hits: 1},
			{Name: "Multiple hits", Hits: 10},
		}

		for _, testCase := range testCases {
			t.Run(testCase.Name, func(t *testing.T) {
				name := t.Name()
				key := fmt.Sprintf("account:%08x", rand.Int())
				peers, err := cluster.ListNonOwningDaemons(name, key)
				require.NoError(t, err)
				owner, err := cluster.FindOwningDaemon(name, key)
				require.NoError(t, err)
				t.Logf("Owner peer: %s", owner.InstanceID)

				require.NoError(t, waitForIdle(1*time.Minute, cluster.GetDaemons()...))

				broadcastCounters := getPeerCounters(t, cluster.GetDaemons(), "gubernator_broadcast_duration_count")
				updateCounters := getPeerCounters(t, cluster.GetDaemons(), "gubernator_global_send_duration_count")
				upgCounters := getPeerCounters(t, cluster.GetDaemons(), "gubernator_grpc_request_duration_count{method=\"/pb.gubernator.PeersV1/UpdatePeerGlobals\"}")
				gprlCounters := getPeerCounters(t, cluster.GetDaemons(), "gubernator_grpc_request_duration_count{method=\"/pb.gubernator.PeersV1/GetPeerRateLimits\"}")

				// When
				for i := int64(0); i < testCase.Hits; i++ {
					sendHit(t, owner, makeReq(name, key, 1), guber.Status_UNDER_LIMIT, 999-i)
				}

				// Then
				// Expect a single global broadcast to all non-owner peers.
				t.Log("Waiting for global broadcasts")
				var wg sync.WaitGroup
				var didOwnerBroadcast, didNonOwnerBroadcast int
				wg.Add(len(peers) + 1)
				go func() {
					expected := broadcastCounters[owner.InstanceID] + 1
					if err := waitForBroadcast(broadcastTimeout, owner, expected); err == nil {
						didOwnerBroadcast++
						t.Log("Global broadcast from owner")
					}
					wg.Done()
				}()
				for _, peer := range peers {
					go func(peer *guber.Daemon) {
						expected := broadcastCounters[peer.InstanceID] + 1
						if err := waitForBroadcast(broadcastTimeout, peer, expected); err == nil {
							didNonOwnerBroadcast++
							t.Logf("Global broadcast from peer %s", peer.InstanceID)
						}
						wg.Done()
					}(peer)
				}
				wg.Wait()
				assert.Equal(t, 1, didOwnerBroadcast)
				assert.Zero(t, didNonOwnerBroadcast)

				// Check for global hits update from non-owner to owner peer.
				// Expect no global hits update because the hits were given
				// directly to the owner peer.
				t.Log("Waiting for global broadcasts")
				var didOwnerUpdate, didNonOwnerUpdate int
				wg.Add(len(peers) + 1)
				go func() {
					expected := updateCounters[owner.InstanceID] + 1
					if err := waitForUpdate(broadcastTimeout, owner, expected); err == nil {
						didOwnerUpdate++
						t.Log("Global hits update from owner")
					}
					wg.Done()
				}()
				for _, peer := range peers {
					go func(peer *guber.Daemon) {
						expected := updateCounters[peer.InstanceID] + 1
						if err := waitForUpdate(broadcastTimeout, peer, expected); err == nil {
							didNonOwnerUpdate++
							t.Logf("Global hits update from peer %s", peer.InstanceID)
						}
						wg.Done()

					}(peer)
				}
				wg.Wait()
				assert.Zero(t, didOwnerUpdate)
				assert.Zero(t, didNonOwnerUpdate)

				// Assert UpdatePeerGlobals endpoint called once on each peer except owner.
				// Used by global broadcast.
				upgCounters2 := getPeerCounters(t, cluster.GetDaemons(), "gubernator_grpc_request_duration_count{method=\"/pb.gubernator.PeersV1/UpdatePeerGlobals\"}")
				for _, peer := range cluster.GetDaemons() {
					expected := upgCounters[peer.InstanceID]
					if peer.PeerInfo.DataCenter == cluster.DataCenterNone && peer.InstanceID != owner.InstanceID {
						expected++
					}
					assert.Equal(t, expected, upgCounters2[peer.InstanceID])
				}

				// Assert PeerGetRateLimits endpoint not called.
				// Used by global hits update.
				gprlCounters2 := getPeerCounters(t, cluster.GetDaemons(), "gubernator_grpc_request_duration_count{method=\"/pb.gubernator.PeersV1/GetPeerRateLimits\"}")
				for _, peer := range cluster.GetDaemons() {
					expected := gprlCounters[peer.InstanceID]
					assert.Equal(t, expected, gprlCounters2[peer.InstanceID])
				}

				// Verify all peers report consistent remaining value value.
				for _, peer := range cluster.GetDaemons() {
					if peer.PeerInfo.DataCenter != cluster.DataCenterNone {
						continue
					}
					sendHit(t, peer, makeReq(name, key, 0), guber.Status_UNDER_LIMIT, limit-testCase.Hits)
				}
			})
		}
	})

	t.Run("Hits on non-owner peer", func(t *testing.T) {
		testCases := []struct {
			Name string
			Hits int64
		}{
			{Name: "Single hit", Hits: 1},
			{Name: "Multiple htis", Hits: 10},
		}

		for _, testCase := range testCases {
			t.Run(testCase.Name, func(t *testing.T) {
				name := t.Name()
				key := fmt.Sprintf("account:%08x", rand.Int())
				peers, err := cluster.ListNonOwningDaemons(name, key)
				require.NoError(t, err)
				owner, err := cluster.FindOwningDaemon(name, key)
				require.NoError(t, err)
				t.Logf("Owner peer: %s", owner.InstanceID)

				require.NoError(t, waitForIdle(1*clock.Minute, cluster.GetDaemons()...))

				broadcastCounters := getPeerCounters(t, cluster.GetDaemons(), "gubernator_broadcast_duration_count")
				updateCounters := getPeerCounters(t, cluster.GetDaemons(), "gubernator_global_send_duration_count")
				upgCounters := getPeerCounters(t, cluster.GetDaemons(), "gubernator_grpc_request_duration_count{method=\"/pb.gubernator.PeersV1/UpdatePeerGlobals\"}")
				gprlCounters := getPeerCounters(t, cluster.GetDaemons(), "gubernator_grpc_request_duration_count{method=\"/pb.gubernator.PeersV1/GetPeerRateLimits\"}")

				// When
				for i := int64(0); i < testCase.Hits; i++ {
					sendHit(t, peers[0], makeReq(name, key, 1), guber.Status_UNDER_LIMIT, 999-i)
				}

				// Then
				// Check for global hits update from non-owner to owner peer.
				// Expect single global hits update from non-owner peer that received hits.
				t.Log("Waiting for global hits updates")
				var wg sync.WaitGroup
				var didOwnerUpdate int
				var didNonOwnerUpdate []string
				wg.Add(len(peers) + 1)
				go func() {
					expected := updateCounters[owner.InstanceID] + 1
					if err := waitForUpdate(broadcastTimeout, owner, expected); err == nil {
						didOwnerUpdate++
						t.Log("Global hits update from owner")
					}
					wg.Done()
				}()
				for _, peer := range peers {
					go func(peer *guber.Daemon) {
						expected := updateCounters[peer.InstanceID] + 1
						if err := waitForUpdate(broadcastTimeout, peer, expected); err == nil {
							didNonOwnerUpdate = append(didNonOwnerUpdate, peer.InstanceID)
							t.Logf("Global hits update from peer %s", peer.InstanceID)
						}
						wg.Done()

					}(peer)
				}
				wg.Wait()
				assert.Zero(t, didOwnerUpdate)
				assert.Len(t, didNonOwnerUpdate, 1)
				assert.Equal(t, []string{peers[0].InstanceID}, didNonOwnerUpdate)

				// Expect a single global broadcast to all non-owner peers.
				t.Log("Waiting for global broadcasts")
				var didOwnerBroadcast, didNonOwnerBroadcast int
				wg.Add(len(peers) + 1)
				go func() {
					expected := broadcastCounters[owner.InstanceID] + 1
					if err := waitForBroadcast(broadcastTimeout, owner, expected); err == nil {
						didOwnerBroadcast++
						t.Log("Global broadcast from owner")
					}
					wg.Done()
				}()
				for _, peer := range peers {
					go func(peer *guber.Daemon) {
						expected := broadcastCounters[peer.InstanceID] + 1
						if err := waitForBroadcast(broadcastTimeout, peer, expected); err == nil {
							didNonOwnerBroadcast++
							t.Logf("Global broadcast from peer %s", peer.InstanceID)
						}
						wg.Done()
					}(peer)
				}
				wg.Wait()
				assert.Equal(t, 1, didOwnerBroadcast)
				assert.Empty(t, didNonOwnerBroadcast)

				// Assert UpdatePeerGlobals endpoint called once on each peer except owner.
				// Used by global broadcast.
				upgCounters2 := getPeerCounters(t, cluster.GetDaemons(), "gubernator_grpc_request_duration_count{method=\"/pb.gubernator.PeersV1/UpdatePeerGlobals\"}")
				for _, peer := range cluster.GetDaemons() {
					expected := upgCounters[peer.InstanceID]
					if peer.PeerInfo.DataCenter == cluster.DataCenterNone && peer.InstanceID != owner.InstanceID {
						expected++
					}
					assert.Equal(t, expected, upgCounters2[peer.InstanceID], "upgCounter %s", peer.InstanceID)
				}

				// Assert PeerGetRateLimits endpoint called once on owner.
				// Used by global hits update.
				gprlCounters2 := getPeerCounters(t, cluster.GetDaemons(), "gubernator_grpc_request_duration_count{method=\"/pb.gubernator.PeersV1/GetPeerRateLimits\"}")
				for _, peer := range cluster.GetDaemons() {
					expected := gprlCounters[peer.InstanceID]
					if peer.InstanceID == owner.InstanceID {
						expected++
					}
					assert.Equal(t, expected, gprlCounters2[peer.InstanceID], "gprlCounter %s", peer.InstanceID)
				}

				// Verify all peers report consistent remaining value value.
				for _, peer := range cluster.GetDaemons() {
					if peer.PeerInfo.DataCenter != cluster.DataCenterNone {
						continue
					}
					sendHit(t, peer, makeReq(name, key, 0), guber.Status_UNDER_LIMIT, limit-testCase.Hits)
				}
			})
		}
	})

	t.Run("Distributed hits", func(t *testing.T) {
		testCases := []struct {
			Name string
			Hits int
		}{
			{Name: "2 hits", Hits: 2},
			{Name: "10 hits", Hits: 10},
			{Name: "100 hits", Hits: 100},
		}

		for _, testCase := range testCases {
			t.Run(testCase.Name, func(t *testing.T) {
				name := t.Name()
				key := fmt.Sprintf("account:%08x", rand.Int())
				peers, err := cluster.ListNonOwningDaemons(name, key)
				require.NoError(t, err)
				owner, err := cluster.FindOwningDaemon(name, key)
				require.NoError(t, err)
				var localPeers []*guber.Daemon
				for _, peer := range cluster.GetDaemons() {
					if peer.PeerInfo.DataCenter == cluster.DataCenterNone && peer.InstanceID != owner.InstanceID {
						localPeers = append(localPeers, peer)
					}
				}
				t.Logf("Owner peer: %s", owner.InstanceID)

				require.NoError(t, waitForIdle(1*clock.Minute, cluster.GetDaemons()...))

				broadcastCounters := getPeerCounters(t, cluster.GetDaemons(), "gubernator_broadcast_duration_count")
				updateCounters := getPeerCounters(t, cluster.GetDaemons(), "gubernator_global_send_duration_count")
				upgCounters := getPeerCounters(t, cluster.GetDaemons(), "gubernator_grpc_request_duration_count{method=\"/pb.gubernator.PeersV1/UpdatePeerGlobals\"}")
				gprlCounters := getPeerCounters(t, cluster.GetDaemons(), "gubernator_grpc_request_duration_count{method=\"/pb.gubernator.PeersV1/GetPeerRateLimits\"}")
				expectUpdate := make(map[string]struct{})
				var wg sync.WaitGroup
				var mutex sync.Mutex

				// When
				wg.Add(testCase.Hits)
				for i := 0; i < testCase.Hits; i++ {
					peer := localPeers[i%len(localPeers)]
					go func(peer *guber.Daemon) {
						sendHit(t, peer, makeReq(name, key, 1), guber.Status_UNDER_LIMIT, -1)
						if peer.InstanceID != owner.InstanceID {
							mutex.Lock()
							expectUpdate[peer.InstanceID] = struct{}{}
							mutex.Unlock()
						}
						wg.Done()
					}(peer)
				}
				wg.Wait()

				// Then
				// Check for global hits update from non-owner to owner peer.
				// Expect single update from each non-owner peer that received
				// hits.
				t.Log("Waiting for global hits updates")
				var didOwnerUpdate int64
				var didNonOwnerUpdate []string
				wg.Add(len(peers) + 1)
				go func() {
					expected := updateCounters[owner.InstanceID] + 1
					if err := waitForUpdate(broadcastTimeout, owner, expected); err == nil {
						atomic.AddInt64(&didOwnerUpdate, 1)
						t.Log("Global hits update from owner")
					}
					wg.Done()
				}()
				for _, peer := range peers {
					go func(peer *guber.Daemon) {
						expected := updateCounters[peer.InstanceID] + 1
						if err := waitForUpdate(broadcastTimeout, peer, expected); err == nil {
							mutex.Lock()
							didNonOwnerUpdate = append(didNonOwnerUpdate, peer.InstanceID)
							mutex.Unlock()
							t.Logf("Global hits update from peer %s", peer.InstanceID)
						}
						wg.Done()

					}(peer)
				}
				wg.Wait()
				assert.Zero(t, didOwnerUpdate)
				assert.Len(t, didNonOwnerUpdate, len(expectUpdate))
				expectedNonOwnerUpdate := maps.Keys(expectUpdate)
				sort.Strings(expectedNonOwnerUpdate)
				sort.Strings(didNonOwnerUpdate)
				assert.Equal(t, expectedNonOwnerUpdate, didNonOwnerUpdate)

				// Expect a single global broadcast to all non-owner peers.
				t.Log("Waiting for global broadcasts")
				var didOwnerBroadcast, didNonOwnerBroadcast int64
				wg.Add(len(peers) + 1)
				go func() {
					expected := broadcastCounters[owner.InstanceID] + 1
					if err := waitForBroadcast(broadcastTimeout, owner, expected); err == nil {
						atomic.AddInt64(&didOwnerBroadcast, 1)
						t.Log("Global broadcast from owner")
					}
					wg.Done()
				}()
				for _, peer := range peers {
					go func(peer *guber.Daemon) {
						expected := broadcastCounters[peer.InstanceID] + 1
						if err := waitForBroadcast(broadcastTimeout, peer, expected); err == nil {
							atomic.AddInt64(&didNonOwnerBroadcast, 1)
							t.Logf("Global broadcast from peer %s", peer.InstanceID)
						}
						wg.Done()
					}(peer)
				}
				wg.Wait()
				assert.Equal(t, int64(1), didOwnerBroadcast)
				assert.Empty(t, didNonOwnerBroadcast)

				// Assert UpdatePeerGlobals endpoint called at least
				// once on each peer except owner.
				// Used by global broadcast.
				upgCounters2 := getPeerCounters(t, cluster.GetDaemons(), "gubernator_grpc_request_duration_count{method=\"/pb.gubernator.PeersV1/UpdatePeerGlobals\"}")
				for _, peer := range cluster.GetDaemons() {
					expected := upgCounters[peer.InstanceID]
					if peer.PeerInfo.DataCenter == cluster.DataCenterNone && peer.InstanceID != owner.InstanceID {
						expected++
					}
					assert.GreaterOrEqual(t, upgCounters2[peer.InstanceID], expected, "upgCounter %s", peer.InstanceID)
				}

				// Assert PeerGetRateLimits endpoint called on owner
				// for each non-owner that received hits.
				// Used by global hits update.
				gprlCounters2 := getPeerCounters(t, cluster.GetDaemons(), "gubernator_grpc_request_duration_count{method=\"/pb.gubernator.PeersV1/GetPeerRateLimits\"}")
				for _, peer := range cluster.GetDaemons() {
					expected := gprlCounters[peer.InstanceID]
					if peer.InstanceID == owner.InstanceID {
						expected += float64(len(expectUpdate))
					}
					assert.Equal(t, expected, gprlCounters2[peer.InstanceID], "gprlCounter %s", peer.InstanceID)
				}

				// Verify all peers report consistent remaining value value.
				for _, peer := range cluster.GetDaemons() {
					if peer.PeerInfo.DataCenter != cluster.DataCenterNone {
						continue
					}
					sendHit(t, peer, makeReq(name, key, 0), guber.Status_UNDER_LIMIT, int64(limit-testCase.Hits))
				}
			})
		}
	})
}

func TestEventChannel(t *testing.T) {
	eventChannel := make(chan guber.HitEvent)
	defer close(eventChannel)
	hits := make(map[string]int64)
	var mu sync.Mutex
	sem := make(chan struct{})
	go func() {
		for e := range eventChannel {
			mu.Lock()
			key := e.Request.Name + "|" + e.Request.UniqueKey
			hits[key] += e.Request.Hits
			mu.Unlock()
			sem <- struct{}{}
		}
	}()

	// Spawn specialized Gubernator cluster with EventChannel enabled.
	cluster.Stop()
	defer func() {
		err := startGubernator()
		require.NoError(t, err)
	}()
	peers := []guber.PeerInfo{
		{GRPCAddress: "127.0.0.1:10000", HTTPAddress: "127.0.0.1:10001", DataCenter: cluster.DataCenterNone},
		{GRPCAddress: "127.0.0.1:10002", HTTPAddress: "127.0.0.1:10003", DataCenter: cluster.DataCenterNone},
		{GRPCAddress: "127.0.0.1:10004", HTTPAddress: "127.0.0.1:10005", DataCenter: cluster.DataCenterNone},
	}
	err := cluster.StartWith(peers, cluster.WithEventChannel(eventChannel))
	require.NoError(t, err)
	defer cluster.Stop()

	client, err := guber.DialV1Server(cluster.GetRandomPeer(cluster.DataCenterNone).GRPCAddress, nil)
	require.NoError(t, err)
	sendHit := func(key string, behavior guber.Behavior) {
		ctx, cancel := context.WithTimeout(context.Background(), clock.Second*10)
		defer cancel()
		_, err = client.GetRateLimits(ctx, &guber.GetRateLimitsReq{
			Requests: []*guber.RateLimitReq{
				{
					Name:      "test",
					UniqueKey: key,
					Algorithm: guber.Algorithm_TOKEN_BUCKET,
					Behavior:  behavior,
					Duration:  guber.Minute * 3,
					Hits:      2,
					Limit:     1000,
				},
			},
		})
		require.NoError(t, err)
		select {
		case <-sem:
		case <-time.After(3 * time.Second):
			t.Fatal("Timeout waiting for EventChannel handler")
		}
	}

	// Send hits using all peering behaviors.
	t.Run("Batching", func(t *testing.T) {
		sendHit("foobar0", guber.Behavior_BATCHING)
		assert.Equal(t, int64(2), hits["test|foobar0"])
	})

	t.Run("No batching", func(t *testing.T) {
		sendHit("foobar1", guber.Behavior_NO_BATCHING)
		assert.Equal(t, int64(2), hits["test|foobar1"])
	})

	t.Run("Global", func(t *testing.T) {
		sendHit("foobar2", guber.Behavior_GLOBAL)
		assert.Equal(t, int64(2), hits["test|foobar2"])
	})
}

// Request metrics and parse into map.
// Optionally pass names to filter metrics by name.
func getMetrics(HTTPAddr string, names ...string) (map[string]*model.Sample, error) {
	url := fmt.Sprintf("http://%s/metrics", HTTPAddr)
	resp, err := http.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP error requesting metrics: %s", resp.Status)
	}
	decoder := expfmt.SampleDecoder{
		Dec: expfmt.NewDecoder(resp.Body, expfmt.FmtText),
		Opts: &expfmt.DecodeOptions{
			Timestamp: model.Now(),
		},
	}
	nameSet := make(map[string]struct{})
	for _, name := range names {
		nameSet[name] = struct{}{}
	}
	metrics := make(map[string]*model.Sample)

	for {
		var smpls model.Vector
		err := decoder.Decode(&smpls)
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		for _, smpl := range smpls {
			name := smpl.Metric.String()
			if _, ok := nameSet[name]; ok || len(nameSet) == 0 {
				metrics[name] = smpl
			}
		}
	}

	return metrics, nil
}

func getMetricRequest(url string, name string) (*model.Sample, error) {
	resp, err := http.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return getMetric(resp.Body, name)
}

func getMetric(in io.Reader, name string) (*model.Sample, error) {
	dec := expfmt.SampleDecoder{
		Dec: expfmt.NewDecoder(in, expfmt.FmtText),
		Opts: &expfmt.DecodeOptions{
			Timestamp: model.Now(),
		},
	}

	var all model.Vector
	for {
		var smpls model.Vector
		err := dec.Decode(&smpls)
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		all = append(all, smpls...)
	}

	for _, s := range all {
		if strings.Contains(s.Metric.String(), name) {
			return s, nil
		}
	}
	return nil, nil
}

// waitForBroadcast waits until the broadcast count for the daemon changes to
// at least the expected value and the broadcast queue is empty.
// Returns an error if timeout waiting for conditions to be met.
func waitForBroadcast(timeout clock.Duration, d *guber.Daemon, expect float64) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	for {
		metrics, err := getMetrics(d.Config().HTTPListenAddress,
			"gubernator_broadcast_duration_count", "gubernator_global_queue_length")
		if err != nil {
			return err
		}
		gbdc := metrics["gubernator_broadcast_duration_count"]
		ggql := metrics["gubernator_global_queue_length"]

		// It's possible a broadcast occurred twice if waiting for multiple
		// peers to forward updates to non-owners.
		if float64(gbdc.Value) >= expect && ggql.Value == 0 {
			return nil
		}

		select {
		case <-clock.After(100 * clock.Millisecond):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// waitForUpdate waits until the global hits update count for the daemon
// changes to at least the expected value and the global update queue is empty.
// Returns an error if timeout waiting for conditions to be met.
func waitForUpdate(timeout clock.Duration, d *guber.Daemon, expect float64) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	for {
		metrics, err := getMetrics(d.Config().HTTPListenAddress,
			"gubernator_global_send_duration_count", "gubernator_global_send_queue_length")
		if err != nil {
			return err
		}
		gsdc := metrics["gubernator_global_send_duration_count"]
		gsql := metrics["gubernator_global_send_queue_length"]

		// It's possible a hit occurred twice if waiting for multiple peers to
		// forward updates to the owner.
		if float64(gsdc.Value) >= expect && gsql.Value == 0 {
			return nil
		}

		select {
		case <-clock.After(100 * clock.Millisecond):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// waitForIdle waits until both global broadcast and global hits queues are
// empty.
func waitForIdle(timeout clock.Duration, daemons ...*guber.Daemon) error {
	var wg syncutil.WaitGroup
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	for _, d := range daemons {
		wg.Run(func(raw any) error {
			d := raw.(*guber.Daemon)
			for {
				metrics, err := getMetrics(d.Config().HTTPListenAddress,
					"gubernator_global_queue_length", "gubernator_global_send_queue_length")
				if err != nil {
					return err
				}
				ggql := metrics["gubernator_global_queue_length"]
				gsql := metrics["gubernator_global_send_queue_length"]

				if ggql.Value == 0 && gsql.Value == 0 {
					return nil
				}

				select {
				case <-clock.After(100 * clock.Millisecond):
				case <-ctx.Done():
					return ctx.Err()
				}
			}
		}, d)
	}
	errs := wg.Wait()
	if len(errs) > 0 {
		return errs[0]
	}
	return nil
}

func getMetricValue(t *testing.T, d *guber.Daemon, name string) float64 {
	m, err := getMetricRequest(fmt.Sprintf("http://%s/metrics", d.Config().HTTPListenAddress),
		name)
	require.NoError(t, err)
	if m == nil {
		return 0
	}
	return float64(m.Value)
}

// Get metric counter values on each peer.
func getPeerCounters(t *testing.T, peers []*guber.Daemon, name string) map[string]float64 {
	counters := make(map[string]float64)
	for _, peer := range peers {
		counters[peer.InstanceID] = getMetricValue(t, peer, name)
	}
	return counters
}

func sendHit(t *testing.T, d *guber.Daemon, req *guber.RateLimitReq, expectStatus guber.Status, expectRemaining int64) {
	if req.Hits != 0 {
		t.Logf("Sending %d hits to peer %s", req.Hits, d.InstanceID)
	}
	client := d.MustClient()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second*10)
	defer cancel()
	resp, err := client.GetRateLimits(ctx, &guber.GetRateLimitsReq{
		Requests: []*guber.RateLimitReq{req},
	})
	require.NoError(t, err)
	item := resp.Responses[0]
	assert.Equal(t, "", item.Error)
	if expectRemaining >= 0 {
		assert.Equal(t, expectRemaining, item.Remaining)
	}
	assert.Equal(t, expectStatus, item.Status)
	assert.Equal(t, req.Limit, item.Limit)
}

func epochMillis(t time.Time) int64 {
	return t.UnixNano() / 1_000_000
}

func startGubernator() error {
	err := cluster.StartWith([]guber.PeerInfo{
		{GRPCAddress: "127.0.0.1:9990", HTTPAddress: "127.0.0.1:9980", DataCenter: cluster.DataCenterNone},
		{GRPCAddress: "127.0.0.1:9991", HTTPAddress: "127.0.0.1:9981", DataCenter: cluster.DataCenterNone},
		{GRPCAddress: "127.0.0.1:9992", HTTPAddress: "127.0.0.1:9982", DataCenter: cluster.DataCenterNone},
		{GRPCAddress: "127.0.0.1:9993", HTTPAddress: "127.0.0.1:9983", DataCenter: cluster.DataCenterNone},
		{GRPCAddress: "127.0.0.1:9994", HTTPAddress: "127.0.0.1:9984", DataCenter: cluster.DataCenterNone},
		{GRPCAddress: "127.0.0.1:9995", HTTPAddress: "127.0.0.1:9985", DataCenter: cluster.DataCenterNone},

		// DataCenterOne
		{GRPCAddress: "127.0.0.1:9890", HTTPAddress: "127.0.0.1:9880", DataCenter: cluster.DataCenterOne},
		{GRPCAddress: "127.0.0.1:9891", HTTPAddress: "127.0.0.1:9881", DataCenter: cluster.DataCenterOne},
		{GRPCAddress: "127.0.0.1:9892", HTTPAddress: "127.0.0.1:9882", DataCenter: cluster.DataCenterOne},
		{GRPCAddress: "127.0.0.1:9893", HTTPAddress: "127.0.0.1:9883", DataCenter: cluster.DataCenterOne},
	})
	if err != nil {
		return errors.Wrap(err, "while starting cluster")
	}

	// Populate peer clients. Avoids data races when goroutines conflict trying
	// to instantiate client singletons.
	for _, peer := range cluster.GetDaemons() {
		_, err = peer.Client()
		if err != nil {
			return errors.Wrap(err, "while connecting client")
		}
	}
	return nil
}

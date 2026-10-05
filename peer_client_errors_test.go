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
	"context"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gubernator-io/gubernator/v2"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// stubPeer is a PeersV1 server that holds every request until release is
// closed, so a test can decide whether the peer is hung or merely slow.
type stubPeer struct {
	gubernator.UnimplementedPeersV1Server
	release  chan struct{}
	received atomic.Int32
	served   atomic.Int32
}

func (s *stubPeer) GetPeerRateLimits(ctx context.Context, req *gubernator.GetPeerRateLimitsReq) (*gubernator.GetPeerRateLimitsResp, error) {
	s.received.Add(1)
	select {
	case <-s.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	s.served.Add(1)

	resp := &gubernator.GetPeerRateLimitsResp{}
	for range req.Requests {
		resp.RateLimits = append(resp.RateLimits, &gubernator.RateLimitResp{Status: gubernator.Status_UNDER_LIMIT})
	}
	return resp, nil
}

func (s *stubPeer) UpdatePeerGlobals(ctx context.Context, _ *gubernator.UpdatePeerGlobalsReq) (*gubernator.UpdatePeerGlobalsResp, error) {
	s.received.Add(1)
	select {
	case <-s.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	s.served.Add(1)
	return &gubernator.UpdatePeerGlobalsResp{}, nil
}

func startStubPeer(t *testing.T) (*stubPeer, string) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	stub := &stubPeer{release: make(chan struct{})}
	server := grpc.NewServer()
	gubernator.RegisterPeersV1Server(server, stub)
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)

	return stub, listener.Addr().String()
}

func newPeerClient(t *testing.T, addr string, behavior gubernator.BehaviorConfig) *gubernator.PeerClient {
	t.Helper()
	logger := logrus.New()
	logger.SetLevel(logrus.ErrorLevel)

	client, err := gubernator.NewPeerClient(gubernator.PeerConfig{
		Info:     gubernator.PeerInfo{GRPCAddress: addr},
		Log:      logrus.NewEntry(logger),
		Behavior: behavior,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Shutdown(context.Background()) })
	return client
}

func rateLimitReq(behavior gubernator.Behavior) *gubernator.RateLimitReq {
	return &gubernator.RateLimitReq{
		Name:      "peer_client_errors",
		UniqueKey: "key",
		Algorithm: gubernator.Algorithm_TOKEN_BUCKET,
		Duration:  gubernator.Second * 60,
		Limit:     100,
		Hits:      1,
		Behavior:  behavior,
	}
}

// A caller that gives up on a forwarded request says nothing about the peer.
// The peer must receive the request, finish it after the caller has gone, and
// end up with no error recorded against it.
func TestCallerCancellationIsNotRecorded(t *testing.T) {
	for _, test := range []struct {
		name     string
		behavior gubernator.Behavior
	}{
		{name: "Batching", behavior: gubernator.Behavior_BATCHING},
		{name: "NoBatching", behavior: gubernator.Behavior_NO_BATCHING},
	} {
		t.Run(test.name, func(t *testing.T) {
			stub, addr := startStubPeer(t)
			client := newPeerClient(t, addr, gubernator.BehaviorConfig{
				BatchWait:    time.Millisecond,
				BatchTimeout: 5 * time.Second,
				BatchLimit:   100,
			})

			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			_, err := client.GetPeerRateLimit(ctx, rateLimitReq(test.behavior))
			require.ErrorIs(t, err, context.DeadlineExceeded)

			// The request was on the wire when the caller left, and the caller
			// leaving did not cancel it on the peer.
			require.Eventually(t, func() bool { return stub.received.Load() == 1 }, time.Second, 10*time.Millisecond)
			assert.Equal(t, int32(0), stub.served.Load())
			assert.Empty(t, client.GetLastErr())

			close(stub.release)
			require.Eventually(t, func() bool { return stub.served.Load() == 1 }, time.Second, 10*time.Millisecond)
			assert.Empty(t, client.GetLastErr())
		})
	}
}

// A peer that never answers within gubernator's own timeout is a fault in the
// peer, whichever path reached it, and is recorded exactly once.
func TestHungPeerIsRecordedOnce(t *testing.T) {
	const timeout = 100 * time.Millisecond
	behavior := gubernator.BehaviorConfig{
		BatchWait:    time.Millisecond,
		BatchTimeout: timeout,
		BatchLimit:   100,
	}

	for _, test := range []struct {
		name string
		call func(*gubernator.PeerClient) error
	}{
		{
			name: "GetPeerRateLimit batching",
			call: func(client *gubernator.PeerClient) error {
				_, err := client.GetPeerRateLimit(context.Background(), rateLimitReq(gubernator.Behavior_BATCHING))
				return err
			},
		},
		{
			name: "GetPeerRateLimit no batching",
			call: func(client *gubernator.PeerClient) error {
				_, err := client.GetPeerRateLimit(context.Background(), rateLimitReq(gubernator.Behavior_NO_BATCHING))
				return err
			},
		},
		{
			// global.go sends hits on a context bounded by GlobalTimeout.
			name: "GetPeerRateLimits on gubernator's own timeout",
			call: func(client *gubernator.PeerClient) error {
				ctx, cancel := context.WithTimeout(context.Background(), timeout)
				defer cancel()
				_, err := client.GetPeerRateLimits(ctx, &gubernator.GetPeerRateLimitsReq{
					Requests: []*gubernator.RateLimitReq{rateLimitReq(gubernator.Behavior_GLOBAL)},
				})
				return err
			},
		},
		{
			// global.go broadcasts on a context bounded by GlobalTimeout.
			name: "UpdatePeerGlobals on gubernator's own timeout",
			call: func(client *gubernator.PeerClient) error {
				ctx, cancel := context.WithTimeout(context.Background(), timeout)
				defer cancel()
				_, err := client.UpdatePeerGlobals(ctx, &gubernator.UpdatePeerGlobalsReq{})
				return err
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			stub, addr := startStubPeer(t)
			client := newPeerClient(t, addr, behavior)

			err := test.call(client)
			require.Equal(t, codes.DeadlineExceeded, status.Code(err))

			require.Equal(t, int32(1), stub.received.Load())
			require.Len(t, client.GetLastErr(), 1)
			assert.Contains(t, client.GetLastErr()[0], "DeadlineExceeded")
		})
	}
}

// A peer that refuses the connection is a fault in the peer, recorded exactly
// once. HealthCheck lists every recorded entry, so a second one for the same
// failure is noise in its message.
func TestUnreachablePeerIsRecordedOnce(t *testing.T) {
	for _, test := range []struct {
		name     string
		behavior gubernator.Behavior
	}{
		{name: "Batching", behavior: gubernator.Behavior_BATCHING},
		{name: "NoBatching", behavior: gubernator.Behavior_NO_BATCHING},
	} {
		t.Run(test.name, func(t *testing.T) {
			client := newPeerClient(t, "127.0.0.1:1", gubernator.BehaviorConfig{
				BatchWait:    time.Millisecond,
				BatchTimeout: time.Second,
				BatchLimit:   100,
			})

			_, err := client.GetPeerRateLimit(context.Background(), rateLimitReq(test.behavior))
			require.ErrorContains(t, err, "connection refused")

			require.Len(t, client.GetLastErr(), 1)
			assert.Contains(t, client.GetLastErr()[0], "connection refused")
		})
	}
}

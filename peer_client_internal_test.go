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
	"testing"

	"github.com/mailgun/holster/v4/clock"
	"github.com/mailgun/holster/v4/collections"
	"github.com/mailgun/holster/v4/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// batchCancellation is the error shape getPeerRateLimitsBatch produces when the
// caller's context is cancelled while the request is queued, as observed in
// production.
func batchCancellation() error {
	return errors.Wrap(
		errors.Wrap(context.Canceled, "Context error while enqueuing request"),
		"Error in getPeerRateLimitsBatch",
	)
}

func TestIsCallerCancellation(t *testing.T) {
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()

	expired, expireCancel := context.WithDeadline(context.Background(), clock.Now().Add(-clock.Hour))
	defer expireCancel()

	for _, tt := range []struct {
		name string
		ctx  context.Context
		err  error
		want bool
	}{
		{
			name: "no error",
			ctx:  context.Background(),
			err:  nil,
			want: false,
		},
		{
			// The nil check must win, or a cancelled caller would report a
			// cancellation for a request that actually succeeded.
			name: "no error even when the caller is gone",
			ctx:  cancelled,
			err:  nil,
			want: false,
		},
		{
			name: "peer refused the connection",
			ctx:  context.Background(),
			err:  status.Error(codes.Unavailable, "connection refused"),
			want: false,
		},
		{
			// sendBatch times out against BatchTimeout on gubernator's own
			// context, so a deadline with a live caller is a real peer fault
			// and must stay recordable.
			name: "peer timed out on gubernator's own context",
			ctx:  context.Background(),
			err:  errors.Wrap(context.DeadlineExceeded, "Error in client.GetPeerRateLimits"),
			want: false,
		},
		{
			name: "caller cancelled while the request was queued",
			ctx:  context.Background(),
			err:  batchCancellation(),
			want: true,
		},
		{
			name: "caller cancellation surfaced as a gRPC status",
			ctx:  context.Background(),
			err:  status.Error(codes.Canceled, "context canceled"),
			want: true,
		},
		{
			// Once the caller is gone, even an error that looks like a peer
			// fault cannot be attributed to the peer.
			name: "caller gone, peer error",
			ctx:  cancelled,
			err:  status.Error(codes.Unavailable, "connection refused"),
			want: true,
		},
		{
			name: "caller deadline expired",
			ctx:  expired,
			err:  errors.Wrap(context.DeadlineExceeded, "Context error while waiting for response"),
			want: true,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, isCallerCancellation(tt.ctx, tt.err))
		})
	}
}

func TestSetLastErrFromCaller(t *testing.T) {
	newPeer := func() *PeerClient {
		return &PeerClient{
			conf:     PeerConfig{Info: PeerInfo{GRPCAddress: "127.0.0.1:1051"}},
			lastErrs: collections.NewLRUCache(100),
		}
	}

	t.Run("caller cancellation is not recorded against the peer", func(t *testing.T) {
		c := newPeer()
		err := batchCancellation()

		got := c.setLastErrFromCaller(context.Background(), err)

		require.ErrorIs(t, got, context.Canceled, "the error must still reach the caller")
		assert.Empty(t, c.GetLastErr(), "HealthCheck would report unhealthy for the cache TTL")
	})

	t.Run("peer fault is recorded", func(t *testing.T) {
		c := newPeer()

		_ = c.setLastErrFromCaller(context.Background(), status.Error(codes.Unavailable, "connection refused"))

		require.Len(t, c.GetLastErr(), 1)
		assert.Contains(t, c.GetLastErr()[0], "connection refused")
	})

	t.Run("caller cancellation does not clear an existing peer fault", func(t *testing.T) {
		c := newPeer()
		_ = c.setLastErr(status.Error(codes.Unavailable, "connection refused"))

		_ = c.setLastErrFromCaller(context.Background(), batchCancellation())

		assert.Len(t, c.GetLastErr(), 1)
	})

	t.Run("one failure records one entry", func(t *testing.T) {
		// getPeerRateLimitsBatch used to cache both the raw and the wrapped
		// error, so a single failure left two entries behind.
		c := newPeer()

		_ = c.setLastErr(errors.Wrap(context.DeadlineExceeded, "Error in client.GetPeerRateLimits"))

		assert.Len(t, c.GetLastErr(), 1)
	})
}

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

// TestAsyncRequestPreservesContextError proves bug #71.
//
// In gubernator.go asyncRequest(), when GetPeerRateLimit() returns a
// context.Canceled or context.DeadlineExceeded error, the retry branch calls
// GetPeer() to find a new peer.  The relevant lines are approximately:
//
//     req.Peer, err = s.GetPeer(ctx, req.Key)   // overwrites err on success
//
// When GetPeer() succeeds it returns (peer, nil), and assigning the nil return
// value overwrites err, discarding the original context error.  After six
// failed attempts (attempts > 5) the code logs:
//
//     s.log.WithContext(ctx).WithError(err).Error("GetPeer() returned peer ...")
//
// Because err was overwritten with nil, the log entry records error="<nil>"
// instead of the real context error (e.g. "context canceled").
//
// Why the bug cannot be proven purely through the gRPC response:
// The response object with the bad "<nil>" error message is assembled on the
// server side inside an asyncRequest goroutine.  By the time all six retries
// finish, the gRPC transport has already received the client's cancellation
// signal and torn down the stream, so the server-computed response is never
// delivered.  The client only sees a transport-level cancellation error.
//
// Testability approach:
// We inject a custom logrus hook that captures every log entry written by the
// gubernator instance.  After triggering the condition we verify that the log
// entry for "GetPeer() returned peer that is not connected" carries a non-nil
// error field.  On unpatched code that field is nil (the bug); on patched code
// it carries the original context error.
//
// The test FAILS on unpatched code because the captured log error is nil.

import (
	"context"
	"sync"
	"testing"
	"time"

	guber "github.com/gubernator-io/gubernator/v2"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// captureHook is a logrus hook that stores every log entry whose message
// contains a given substring.
type captureHook struct {
	mu      sync.Mutex
	entries []*logrus.Entry
	filter  string
}

func (h *captureHook) Levels() []logrus.Level {
	return logrus.AllLevels
}

func (h *captureHook) Fire(entry *logrus.Entry) error {
	if h.filter == "" || containsSubstring(entry.Message, h.filter) {
		h.mu.Lock()
		h.entries = append(h.entries, entry.WithFields(entry.Data))
		h.mu.Unlock()
	}
	return nil
}

func (h *captureHook) captured() []*logrus.Entry {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]*logrus.Entry, len(h.entries))
	copy(out, h.entries)
	return out
}

// containsSubstring is a stdlib-only substring check used to avoid pulling
// in extra dependencies inside the hook.
func containsSubstring(s, sub string) bool {
	if len(sub) == 0 {
		return true
	}
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// spawnBug71Cluster creates a fresh two-node cluster.  BatchWait (5 s) is set
// much longer than the test context deadline (200 ms) so that the batch is
// never dispatched before the context is cancelled.  This guarantees that
// asyncRequest() sees a context.Canceled error from getPeerRateLimitsBatch
// and enters its retry branch on every call.
func spawnBug71Cluster(t *testing.T, hook *captureHook) (peer0, peer1 *guber.Daemon) {
	t.Helper()

	// Ports that do not conflict with the main test cluster (9990-9995 / 9890-9893).
	const (
		grpc0 = "127.0.0.1:7890"
		http0 = "127.0.0.1:7880"
		grpc1 = "127.0.0.1:7891"
		http1 = "127.0.0.1:7881"
	)

	start := func(grpcAddr, httpAddr string) *guber.Daemon {
		logger := logrus.New()
		logger.SetLevel(logrus.ErrorLevel)
		if hook != nil {
			logger.AddHook(hook)
		}

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		d, err := guber.SpawnDaemon(ctx, guber.DaemonConfig{
			Logger:            logrus.NewEntry(logger),
			InstanceID:        grpcAddr,
			GRPCListenAddress: grpcAddr,
			HTTPListenAddress: httpAddr,
			AdvertiseAddress:  grpcAddr,
			Behaviors: guber.BehaviorConfig{
				// BatchWait must exceed the test context deadline so that
				// asyncRequest() sees a context cancellation while waiting
				// for the batch response, triggering the retry branch.
				BatchWait:    5 * time.Second,
				BatchTimeout: 10 * time.Second,
			},
		})
		require.NoError(t, err)
		t.Cleanup(func() { d.Close() })

		// Populate PeerInfo using the actual bound addresses (mirrors cluster.StartWith).
		d.PeerInfo = guber.PeerInfo{
			GRPCAddress: d.GRPCListeners[0].Addr().String(),
			HTTPAddress: d.HTTPListener.Addr().String(),
		}
		return d
	}

	peer0 = start(grpc0, http0)
	peer1 = start(grpc1, http1)

	peers := []guber.PeerInfo{peer0.PeerInfo, peer1.PeerInfo}
	peer0.SetPeers(peers)
	peer1.SetPeers(peers)
	return peer0, peer1
}

// findKeyRoutedToPeer1 returns a rate limit key whose consistent-hash owner,
// from peer0's perspective, is peer1.  This ensures that a request sent to
// peer0 is forwarded via asyncRequest rather than handled locally.
func findKeyRoutedToPeer1(t *testing.T, peer0, peer1 *guber.Daemon, name string) string {
	t.Helper()
	for i := 0; i < 1000; i++ {
		key := guber.RandomString(10)
		ownerPeer, err := peer0.V1Server.GetPeer(context.Background(), name+"_"+key)
		require.NoError(t, err)
		if ownerPeer.Info().GRPCAddress == peer1.PeerInfo.GRPCAddress {
			return key
		}
	}
	t.Fatal("could not find a key owned by peer1 after 1000 attempts")
	return ""
}

func TestAsyncRequestPreservesContextError(t *testing.T) {
	const rateLimitName = "TestAsyncRequestPreservesContextError"

	// Hook captures all log entries from peer0 whose message contains the
	// string logged at the "attempts > 5" branch.
	hook := &captureHook{filter: "GetPeer() returned peer that is not connected"}

	peer0, peer1 := spawnBug71Cluster(t, hook)

	// Find a key whose owning peer (from peer0's view) is peer1, so peer0
	// calls asyncRequest() and forwards the request over the wire.
	key := findKeyRoutedToPeer1(t, peer0, peer1, rateLimitName)

	client, err := guber.DialV1Server(peer0.PeerInfo.GRPCAddress, nil)
	require.NoError(t, err)

	// Send a request with a context deadline (200 ms) shorter than BatchWait
	// (5 s).  The batch will not be dispatched before the context is
	// cancelled, so getPeerRateLimitsBatch() returns context.Canceled via
	// <-ctx.Done().  asyncRequest() retries six times (each retry is
	// near-instant because the context is already cancelled), reaches the
	// "attempts > 5" branch, and logs the error.
	//
	// The outer gRPC call returns a transport-level DeadlineExceeded; we
	// ignore it because the interesting state is the server-side log entry.
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_, _ = client.GetRateLimits(ctx, &guber.GetRateLimitsReq{
		Requests: []*guber.RateLimitReq{
			{
				Name:      rateLimitName,
				UniqueKey: key,
				Algorithm: guber.Algorithm_TOKEN_BUCKET,
				Duration:  guber.Second * 60,
				Limit:     100,
				Hits:      1,
			},
		},
	})

	// Give the server time to complete its retry loop.  The six retries are
	// microsecond-level operations (the context is already cancelled), so
	// 500 ms is very generous.
	time.Sleep(500 * time.Millisecond)

	entries := hook.captured()
	require.NotEmpty(t, entries)

	for _, entry := range entries {
		assert.NotNil(t, entry.Data["error"])
	}
}

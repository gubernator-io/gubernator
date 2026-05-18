package gubernator_test

import (
	"context"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	guber "github.com/gubernator-io/gubernator/v2"
	"github.com/mailgun/holster/v4/clock"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
)

type slowPeersV1Server struct {
	guber.UnimplementedPeersV1Server
}

func (s *slowPeersV1Server) GetPeerRateLimits(ctx context.Context, _ *guber.GetPeerRateLimitsReq) (*guber.GetPeerRateLimitsResp, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

type fixedPeerPicker struct {
	peer *guber.PeerClient
}

func (p *fixedPeerPicker) GetByPeerInfo(guber.PeerInfo) *guber.PeerClient { return p.peer }
func (p *fixedPeerPicker) Peers() []*guber.PeerClient                     { return []*guber.PeerClient{p.peer} }
func (p *fixedPeerPicker) Get(string) (*guber.PeerClient, error)          { return p.peer, nil }
func (p *fixedPeerPicker) New() guber.PeerPicker                          { return p }
func (p *fixedPeerPicker) Add(*guber.PeerClient)                          {}

func TestErrorStompedOnGetPeerRetry(t *testing.T) {
	// Start a fake peer GRPC server that blocks until context is canceled.
	fakePeerListener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	fakePeerSrv := grpc.NewServer()
	guber.RegisterPeersV1Server(fakePeerSrv, &slowPeersV1Server{})
	go func() {
		if err := fakePeerSrv.Serve(fakePeerListener); err != nil {
			fmt.Printf("while serving fake peer: %s\n", err)
		}
	}()
	defer fakePeerSrv.GracefulStop()

	// Create a PeerClient with batching enabled and a long batch timeout.
	// The batch timeout (30s) must be much longer than the caller's context
	// deadline, so the caller's context expires first. This causes
	// getPeerRateLimitsBatch to return via ctx.Done(), producing an error
	// where errors.Is(err, context.DeadlineExceeded) == true, which
	// enters the retry loop in asyncRequest.
	peer, err := guber.NewPeerClient(guber.PeerConfig{
		Info: guber.PeerInfo{
			GRPCAddress: fakePeerListener.Addr().String(),
			IsOwner:     false,
		},
		Behavior: guber.BehaviorConfig{
			BatchTimeout: time.Second * 30,
			BatchWait:    time.Millisecond * 1,
			BatchLimit:   1,
		},
		Log: logrus.WithField("test", "issue71"),
	})
	require.NoError(t, err)

	picker := &fixedPeerPicker{peer: peer}

	mainSrv := grpc.NewServer()
	srv, err := guber.NewV1Instance(guber.Config{
		GRPCServers: []*grpc.Server{mainSrv},
		LocalPicker: picker,
		Behaviors: guber.BehaviorConfig{
			GlobalSyncWait: clock.Millisecond * 50,
			GlobalTimeout:  clock.Second,
		},
	})
	require.NoError(t, err)
	defer func() { _ = srv.Close() }()

	mainListener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	go func() {
		if err := mainSrv.Serve(mainListener); err != nil {
			fmt.Printf("while serving main: %s\n", err)
		}
	}()
	defer mainSrv.GracefulStop()

	// Call GetRateLimits directly on the V1Instance (exported method).
	// Use a short context deadline so the caller's context expires during
	// getPeerRateLimitsBatch, triggering the retry loop. Since we're calling
	// the method directly (not through GRPC), we can observe the response
	// even after the context expires — GetRateLimits blocks on wg.Wait()
	// until asyncRequest completes.
	shortCtx, cancel := context.WithTimeout(context.Background(), time.Millisecond*100)
	defer cancel()

	createdAt := int64(0)
	resp, err := srv.GetRateLimits(shortCtx, &guber.GetRateLimitsReq{
		Requests: []*guber.RateLimitReq{
			{
				Name:      "test_error_stomp",
				UniqueKey: "account:123",
				Algorithm: guber.Algorithm_TOKEN_BUCKET,
				Duration:  guber.Second,
				Hits:      1,
				Limit:     10,
				CreatedAt: &createdAt,
			},
		},
	})
	require.NoError(t, err)
	require.Len(t, resp.Responses, 1)

	// The response should have an error because the peer is unreachable.
	// BUG: Due to the error stomp at gubernator.go:368, the error wraps nil,
	// producing "...peers that are not connected for '...': <nil>" instead
	// of containing the original context error cause.
	errMsg := resp.Responses[0].Error
	assert.NotEmpty(t, errMsg)
	assert.False(t, strings.Contains(errMsg, "<nil>"),
		"error message should not contain '<nil>' — the original context error was stomped")
	assert.True(t,
		strings.Contains(strings.ToLower(errMsg), "deadline exceeded") ||
			strings.Contains(strings.ToLower(errMsg), "canceled"),
		"error message should contain the original context error cause")
}

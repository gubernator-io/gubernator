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
	"runtime"
	"testing"
	"time"

	guber "github.com/gubernator-io/gubernator/v2"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// An embedder can call the policy methods on an instance with the adapter
// disabled; they must answer Unimplemented like the peer methods do, not panic.
func TestPolicyMethodsUnimplementedWhenEnvoyDisabled(t *testing.T) {
	instance, err := guber.NewV1Instance(guber.Config{GRPCServers: []*grpc.Server{grpc.NewServer()}})
	require.NoError(t, err)
	defer func() { _ = instance.Close() }()
	ctx := context.Background()

	_, err = instance.ApplyPolicies(ctx, &guber.ApplyPoliciesReq{Policies: []*guber.DomainPolicy{{Domain: "d"}}})
	assert.Equal(t, codes.Unimplemented, status.Code(err))
	_, err = instance.DeletePolicies(ctx, &guber.DeletePoliciesReq{Domains: []string{"d"}})
	assert.Equal(t, codes.Unimplemented, status.Code(err))
	_, err = instance.ListPolicies(ctx, &guber.ListPoliciesReq{})
	assert.Equal(t, codes.Unimplemented, status.Code(err))
}

// Enabling the adapter without a RateLimitService registrar would leave the
// policy API live and Envoy with nothing to call; NewV1Instance must refuse it
// the same way SpawnDaemon does.
func TestNewV1InstanceRequiresRegisterRLSWhenEnabled(t *testing.T) {
	_, err := guber.NewV1Instance(guber.Config{
		GRPCServers: []*grpc.Server{grpc.NewServer()},
		Envoy:       guber.EnvoyConfig{Enabled: true},
	})
	require.ErrorContains(t, err, "RegisterRLS")
}

// Close drains the bootstrap pull started by SetPeers (through the peer
// client's in-flight request tracking) rather than returning while it runs.
func TestCloseWaitsForBootstrapPull(t *testing.T) {
	// A listener that accepts and never answers holds the pull open until GlobalTimeout
	blackhole, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer func() { _ = blackhole.Close() }()
	go func() {
		for {
			conn, err := blackhole.Accept()
			if err != nil {
				return
			}
			defer func() { _ = conn.Close() }()
		}
	}()

	instance, err := guber.NewV1Instance(guber.Config{
		GRPCServers:   []*grpc.Server{grpc.NewServer()},
		AdvertiseAddr: "127.0.0.1:1",
		Behaviors:     guber.BehaviorConfig{GlobalTimeout: 3 * time.Second},
		Envoy: guber.EnvoyConfig{
			Enabled:     true,
			RegisterRLS: func([]*grpc.Server, *guber.V1Instance) prometheus.Collector { return nil },
		},
	})
	require.NoError(t, err)
	instance.SetPeers([]guber.PeerInfo{
		{GRPCAddress: "127.0.0.1:1", IsOwner: true},
		{GRPCAddress: blackhole.Addr().String()},
	})

	require.NoError(t, instance.Close())
	stacks := make([]byte, 1<<20)
	stacks = stacks[:runtime.Stack(stacks, true)]
	assert.NotContains(t, string(stacks), "envoyPolicyManager).pull")
}

// Without an advertise address the instance id identifies the applying peer.
func TestOriginFallsBackToInstanceID(t *testing.T) {
	instance, err := guber.NewV1Instance(guber.Config{
		GRPCServers: []*grpc.Server{grpc.NewServer()},
		InstanceID:  "instance-7",
		Envoy: guber.EnvoyConfig{
			Enabled:     true,
			RegisterRLS: func([]*grpc.Server, *guber.V1Instance) prometheus.Collector { return nil },
		},
	})
	require.NoError(t, err)
	defer func() { _ = instance.Close() }()

	resp, err := instance.ApplyPolicies(context.Background(), &guber.ApplyPoliciesReq{
		Policies: []*guber.DomainPolicy{{Domain: "d"}},
	})
	require.NoError(t, err)
	assert.Equal(t, "instance-7", resp.Applied[0].Origin)
}

// rogueEnvoyPeer answers GetPeerPolicies with whatever it is given, so a test
// can hand an instance entries a conforming peer would never produce.
type rogueEnvoyPeer struct {
	guber.UnimplementedPeersV1Server
	policies []*guber.PeerPolicy
}

func (r *rogueEnvoyPeer) GetPeerPolicies(context.Context, *guber.GetPeerPoliciesReq) (*guber.GetPeerPoliciesResp, error) {
	return &guber.GetPeerPoliciesResp{Policies: r.policies}, nil
}

// A pull keeps the entries that pass validation and drops the rest, so one
// malformed entry from a peer cannot poison the store or block the others.
func TestPullSkipsInvalidEntriesFromPeer(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	rogue := grpc.NewServer()
	guber.RegisterPeersV1Server(rogue, &rogueEnvoyPeer{policies: []*guber.PeerPolicy{
		{Policy: &guber.DomainPolicy{Domain: "bad", Version: 0, Origin: "rogue"}},
		{Policy: &guber.DomainPolicy{Domain: "good", Version: 5, Origin: "rogue", OnMissingLimit: guber.MissingLimitAction_ALLOW}},
	}})
	go func() { _ = rogue.Serve(listener) }()
	defer rogue.Stop()

	instance, err := guber.NewV1Instance(guber.Config{
		GRPCServers:   []*grpc.Server{grpc.NewServer()},
		AdvertiseAddr: "127.0.0.1:1",
		Behaviors:     guber.BehaviorConfig{GlobalTimeout: 3 * time.Second},
		Envoy: guber.EnvoyConfig{
			Enabled:     true,
			RegisterRLS: func([]*grpc.Server, *guber.V1Instance) prometheus.Collector { return nil },
		},
	})
	require.NoError(t, err)
	defer func() { _ = instance.Close() }()
	instance.SetPeers([]guber.PeerInfo{
		{GRPCAddress: "127.0.0.1:1", IsOwner: true},
		{GRPCAddress: listener.Addr().String()},
	})

	require.Eventually(t, func() bool {
		resp, err := instance.ListPolicies(context.Background(), &guber.ListPoliciesReq{})
		require.NoError(t, err)
		return len(resp.Policies) == 1 && resp.Policies[0].Domain == "good"
	}, 5*time.Second, 50*time.Millisecond)
}

package gubernator_test

import (
	"context"
	"maps"
	"net"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gubernator-io/gubernator/v2"
	"github.com/miekg/dns"
	"github.com/pkg/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
)

func fakeDNSServer(t testing.TB, hosts map[string][]string) (string, error) {
	t.Helper()

	l, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	t.Cleanup(func() {
		_ = l.Close()
	})

	started := make(chan struct{})
	s := &dns.Server{
		PacketConn: l,
		NotifyStartedFunc: func() {
			close(started)
		},
		Handler: dns.HandlerFunc(func(w dns.ResponseWriter, r *dns.Msg) {
			host := r.Question[0].Name
			m := new(dns.Msg)
			m.SetReply(r)

			if ips, ok := hosts[host]; ok {
				if r.Question[0].Qtype == dns.TypeA {
					for _, ip := range ips {
						m.Answer = append(m.Answer, &dns.A{
							Hdr: dns.RR_Header{
								Name:   host,
								Rrtype: dns.TypeA,
								Class:  dns.ClassINET,
								Ttl:    100,
							},
							A: net.ParseIP(ip),
						})
					}
				}
			} else {
				m.Rcode = dns.RcodeNameError
			}
			if err := w.WriteMsg(m); err != nil {
				t.Errorf("Error writing response: %v", err)
			}
		}),
	}

	go func() {
		_ = s.ActivateAndServe()
	}()
	t.Cleanup(func() {
		_ = s.Shutdown()
	})

	select {
	case <-started:
	case <-time.After(5 * time.Second):
		return "", errors.New("timed out waiting for server to start")
	}

	return l.LocalAddr().String(), nil
}

func TestNewDNSPool(t *testing.T) {
	ownIP := "10.45.0.1"
	peerIPs := map[string][]string{
		"local-cluster-0.gubernator.io.": {
			ownIP,
			"10.45.0.2",
			"10.45.0.3",
		},
		"local-cluster-1.gubernator.io.": {
			"10.45.1.1",
			"10.45.1.2",
			"10.45.1.3",
		},
		"local-cluster-2.gubernator.io.": {
			"10.45.2.1",
			"10.45.2.2",
			"10.45.2.3",
		},
	}

	addr, err := fakeDNSServer(t, peerIPs)
	require.NoError(t, err)

	peersInfo := make(chan []gubernator.PeerInfo)
	ownGRPCAddress := net.JoinHostPort(ownIP, "1051")
	pool, err := gubernator.NewDNSPool(gubernator.DNSPoolConfig{
		FQDN:          strings.Join(slices.Collect(maps.Keys(peerIPs)), ","),
		ResolvServers: []string{addr},
		OwnAddress:    ownGRPCAddress,
		OnUpdate: func(peers []gubernator.PeerInfo) {
			peersInfo <- peers
		},
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		pool.Close() // stop goroutine before closing channel it sends to
		close(peersInfo)
	})

	select {
	case peers := <-peersInfo:
		expectedIPs := slices.Concat(slices.Collect(maps.Values(peerIPs))...)
		assert.Len(t, expectedIPs, len(peers))
		for _, peer := range peers {
			ip, _, _ := net.SplitHostPort(peer.GRPCAddress)
			assert.Contains(t, expectedIPs, ip)
			assert.Equal(t, peer.GRPCAddress == ownGRPCAddress, peer.IsOwner)
			// No DataCenter configured, so all peers should have empty DataCenter
			assert.Empty(t, peer.DataCenter)
		}

	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for peers")
	}
}

func TestNewDNSPoolWithDataCenter(t *testing.T) {
	ownIP := "10.45.0.1"
	localFQDN := "local-cluster-0.gubernator.io."
	peerIPs := map[string][]string{
		localFQDN: {
			ownIP,
			"10.45.0.2",
			"10.45.0.3",
		},
		"local-cluster-1.gubernator.io.": {
			"10.45.1.1",
			"10.45.1.2",
			"10.45.1.3",
		},
		"local-cluster-2.gubernator.io.": {
			"10.45.2.1",
			"10.45.2.2",
			"10.45.2.3",
		},
	}

	addr, err := fakeDNSServer(t, peerIPs)
	require.NoError(t, err)

	peersInfo := make(chan []gubernator.PeerInfo)
	ownGRPCAddress := net.JoinHostPort(ownIP, "1051")
	pool, err := gubernator.NewDNSPool(gubernator.DNSPoolConfig{
		FQDN:          strings.Join(slices.Collect(maps.Keys(peerIPs)), ","),
		ResolvServers: []string{addr},
		OwnAddress:    ownGRPCAddress,
		DataCenter:    localFQDN,
		OnUpdate: func(peers []gubernator.PeerInfo) {
			peersInfo <- peers
		},
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		pool.Close() // stop goroutine before closing channel it sends to
		close(peersInfo)
	})

	select {
	case peers := <-peersInfo:
		expectedIPs := slices.Concat(slices.Collect(maps.Values(peerIPs))...)
		assert.Len(t, expectedIPs, len(peers))
		for _, peer := range peers {
			ip, _, _ := net.SplitHostPort(peer.GRPCAddress)
			t.Logf("DataCenter=%q Address=%q IsOwner=%v", peer.DataCenter, peer.GRPCAddress, peer.IsOwner)
			assert.Contains(t, expectedIPs, ip)
			assert.Equal(t, peer.GRPCAddress == ownGRPCAddress, peer.IsOwner)
			// DataCenter must be set to the FQDN the peer was resolved from
			assert.NotEmpty(t, peer.DataCenter)
		}

	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for peers")
	}
}

// newDNSInstance starts a real V1Instance on a random port and returns the instance,
// its listening address, and a cleanup function.
func newDNSInstance(t *testing.T, dataCenter string) (*gubernator.V1Instance, string) {
	t.Helper()

	grpcSrv := grpc.NewServer()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	grpcAddr := l.Addr().String()

	srv, err := gubernator.NewV1Instance(gubernator.Config{
		GRPCServers:   []*grpc.Server{grpcSrv},
		AdvertiseAddr: grpcAddr,
		DataCenter:    dataCenter,
	})
	require.NoError(t, err)

	go func() { _ = grpcSrv.Serve(l) }()
	t.Cleanup(func() {
		grpcSrv.GracefulStop()
		_ = srv.Close()
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, gubernator.WaitForConnect(ctx, []string{grpcAddr}))

	return srv, grpcAddr
}

func TestDNSPoolHealthCheck(t *testing.T) {
	// End-to-end regression test for the primary bug: when GUBER_DATA_CENTER is
	// not set (DataCenter=""), the DNS pool must place all peers in LocalPicker.
	// Before the fix, peers always got DataCenter=fqdn, so none matched
	// conf.DataCenter="" — the LocalPicker was empty and HealthCheck returned
	// "this instance is not found in the peer list".
	srv, grpcAddr := newDNSInstance(t, "")
	ownIP, _, _ := net.SplitHostPort(grpcAddr)

	dnsAddr, err := fakeDNSServer(t, map[string][]string{
		"my-cluster.example.com.": {ownIP},
	})
	require.NoError(t, err)

	pool, err := gubernator.NewDNSPool(gubernator.DNSPoolConfig{
		FQDN:          "my-cluster.example.com.",
		ResolvServers: []string{dnsAddr},
		OwnAddress:    grpcAddr,
		DataCenter:    "", // no datacenter — all peers get DataCenter="" → LocalPicker
		OnUpdate:      srv.SetPeers,
	})
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	client, err := gubernator.DialV1Server(grpcAddr, nil)
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		resp, err := client.HealthCheck(context.Background(), &gubernator.HealthCheckReq{})
		return err == nil && resp.Status == "healthy"
	}, 5*time.Second, 100*time.Millisecond)
}

func TestDNSPoolFQDNNormalization(t *testing.T) {
	// Validates that FQDNs are normalized to canonical form (trailing dot) regardless
	// of how the operator configures GUBER_DNS_FQDN. This matters because peer.DataCenter
	// is set to the FQDN string and must exactly match GUBER_DATA_CENTER in SetPeers
	// routing. For daemon users, config.go also normalizes GUBER_DATA_CENTER to the
	// same canonical form, so operators need not worry about trailing-dot consistency
	// between the two env vars.
	peerIPs := map[string][]string{
		"my-cluster.example.com.": {"10.0.0.1", "10.0.0.2"}, // fakeDNSServer keyed by canonical form
	}
	dnsAddr, err := fakeDNSServer(t, peerIPs)
	require.NoError(t, err)

	peersInfo := make(chan []gubernator.PeerInfo, 1)
	pool, err := gubernator.NewDNSPool(gubernator.DNSPoolConfig{
		FQDN:          "my-cluster.example.com", // no trailing dot
		ResolvServers: []string{dnsAddr},
		OwnAddress:    "10.0.0.1:1051",
		DataCenter:    "my-cluster.example.com", // no trailing dot (non-empty enables multi-DC mode)
		OnUpdate:      func(peers []gubernator.PeerInfo) { peersInfo <- peers },
	})
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	select {
	case peers := <-peersInfo:
		for _, peer := range peers {
			// Pool normalizes FQDN to canonical form; peer.DataCenter always has trailing dot
			assert.Equal(t, "my-cluster.example.com.", peer.DataCenter)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for peers")
	}
}

func TestDNSPoolMultiDataCenterHealthCheck(t *testing.T) {
	// End-to-end test for multi-datacenter DNS discovery. Each FQDN becomes the
	// datacenter name for the peers resolved from it. The local FQDN's peers land
	// in LocalPicker; remote FQDNs' peers land in RegionPicker. HealthCheck must
	// find the own instance in LocalPicker to report healthy.
	const localFQDN = "local-cluster.example.com."
	const remoteFQDN = "remote-cluster.example.com."

	srv, grpcAddr := newDNSInstance(t, localFQDN)
	ownIP, _, _ := net.SplitHostPort(grpcAddr)

	dnsAddr, err := fakeDNSServer(t, map[string][]string{
		localFQDN:  {ownIP},
		remoteFQDN: {"10.1.0.1", "10.1.0.2"}, // remote peers; grpc.NewClient dials lazily
	})
	require.NoError(t, err)

	pool, err := gubernator.NewDNSPool(gubernator.DNSPoolConfig{
		FQDN:          strings.Join([]string{localFQDN, remoteFQDN}, ","),
		ResolvServers: []string{dnsAddr},
		OwnAddress:    grpcAddr,
		DataCenter:    localFQDN, // non-empty → each peer gets its FQDN as DataCenter
		OnUpdate:      srv.SetPeers,
	})
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	client, err := gubernator.DialV1Server(grpcAddr, nil)
	require.NoError(t, err)

	// Wait for the DNS pool to populate both pickers.
	var resp *gubernator.HealthCheckResp
	require.Eventually(t, func() bool {
		resp, err = client.HealthCheck(context.Background(), &gubernator.HealthCheckReq{})
		return err == nil && resp.Status == "healthy"
	}, 5*time.Second, 100*time.Millisecond)

	// Local FQDN resolves to 1 peer (our own instance).
	assert.Len(t, resp.LocalPeers, 1)
	assert.Equal(t, localFQDN, resp.LocalPeers[0].DataCenter)

	// Remote FQDN resolves to 2 peers.
	assert.Len(t, resp.RegionPeers, 2)
	for _, p := range resp.RegionPeers {
		assert.Equal(t, remoteFQDN, p.DataCenter)
	}
}

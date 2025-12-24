package gubernator_test

import (
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
		pool.Close()
	})
	t.Cleanup(func() {
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
		}

	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for peers")
	}
}

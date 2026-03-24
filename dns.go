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
	"math/rand"
	"net"
	"os"
	"strings"
	"time"

	"github.com/mailgun/holster/v4/setter"
	"github.com/miekg/dns"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
)

// DNSResolver represents a dns resolver
type DNSResolver struct {
	Servers []string
	random  *rand.Rand
}

// NewFromResolvConf initializes DnsResolver from resolv.conf like file.
func NewFromResolvConf(path string) (*DNSResolver, error) {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return &DNSResolver{}, errors.New("no such file or directory: " + path)
	}

	config, err := dns.ClientConfigFromFile(path)
	if err != nil {
		return &DNSResolver{}, err
	}

	var servers []string
	if len(config.Servers) == 0 {
		return &DNSResolver{}, errors.New("no servers in config")
	}
	for _, ipAddress := range config.Servers {
		servers = append(servers, net.JoinHostPort(ipAddress, "53"))
	}
	return &DNSResolver{servers, rand.New(rand.NewSource(time.Now().UnixNano()))}, nil
}

func (r *DNSResolver) lookupHost(host string, dnsType uint16) ([]net.IP, uint32, error) {
	m1 := new(dns.Msg)
	m1.Id = dns.Id()
	m1.RecursionDesired = true
	m1.Question = []dns.Question{{Name: dns.Fqdn(host), Qtype: dnsType, Qclass: dns.ClassINET}}

	in, err := dns.Exchange(m1, r.Servers[r.random.Intn(len(r.Servers))])
	if err != nil {
		return nil, 0, err
	}

	if in.Rcode != dns.RcodeSuccess {
		return nil, 0, errors.New(dns.RcodeToString[in.Rcode])
	}

	ttl := defaultPollDelay
	result := make([]net.IP, 0, len(in.Answer))
	for _, record := range in.Answer {
		switch r := record.(type) {
		case *dns.A:
			result = append(result, r.A)
		case *dns.AAAA:
			result = append(result, r.AAAA)
		}
		ttl = min(ttl, record.Header().Ttl)
	}

	return result, ttl, nil
}

func NewDNSResolver(servers []string) (*DNSResolver, error) {
	return &DNSResolver{Servers: servers, random: rand.New(rand.NewSource(time.Now().UnixNano()))}, nil
}

type DNSPoolConfig struct {
	// (Required) The FQDN that should resolve to gubernator instance ip addresses
	FQDN string

	// (Required) Filesystem path to "/etc/resolv.conf"
	// Only used if ResolvServers is not provided.
	ResolvConf string

	// (Optional) List of resolvers to use, will override ResolvConf if provided
	// Defaults to the servers in ResolvConf
	ResolvServers []string

	// (Required) Own GRPC address
	OwnAddress string

	// (Required) Called when the list of gubernators in the pool updates
	OnUpdate UpdateFunc

	// (Optional) Identifies the local cluster when using multi-datacenter DNS
	// discovery. Its behavior differs fundamentally from etcd and member-list.
	//
	// With etcd and member-list, DataCenter is arbitrary metadata that each
	// peer explicitly advertises to the others (e.g., "us-east-1"). Any
	// consistent string works because peers exchange the value directly.
	//
	// With DNS, there is no metadata exchange. Instead, the FQDN that a peer's
	// IP was resolved from becomes that peer's datacenter name. DataCenter must
	// therefore be set to the exact FQDN of the local cluster so that SetPeers
	// can distinguish local peers (same FQDN → LocalPicker) from remote peers
	// (different FQDN → RegionPicker).
	//
	// Leave empty (default) for single-datacenter deployments. All discovered
	// peers will be treated as local regardless of which FQDN they came from.
	//
	// Example multi-datacenter setup with three clouds:
	//
	//   GUBER_DATA_CENTER=gubernator.svc.eks-cluster.
	//   GUBER_DNS_FQDN=gubernator.svc.eks-cluster.,gubernator.svc.gke-cluster.,gubernator.svc.aks-cluster.
	//
	// Peers resolved from gubernator.svc.eks-cluster. get DataCenter set to
	// that FQDN and land in the LocalPicker. Peers from the other FQDNs get
	// their respective FQDNs as DataCenter and land in the RegionPicker.
	DataCenter string

	Logger FieldLogger
}

type DNSPool struct {
	log        FieldLogger
	ctx        context.Context
	cancel     context.CancelFunc
	resolver   *DNSResolver
	fqdns      []string // list of FQDNs to resolve
	ownIP      string
	ownPort    string
	onUpdate   UpdateFunc
	dataCenter string // when non-empty, use FQDNs as DC names
}

func newResolver(conf DNSPoolConfig) (*DNSResolver, error) {
	if len(conf.ResolvServers) > 0 {
		return NewDNSResolver(conf.ResolvServers)
	}
	return NewFromResolvConf(conf.ResolvConf)
}

func NewDNSPool(conf DNSPoolConfig) (*DNSPool, error) {
	setter.SetDefault(&conf.Logger, logrus.WithField("category", "gubernator"))

	if conf.OwnAddress == "" {
		return nil, errors.New("Advertise.GRPCAddress is required")
	}
	ip, port, err := net.SplitHostPort(conf.OwnAddress)
	if err != nil {
		return nil, errors.Wrap(err, "OwnAddress is invalid")
	}
	if port == "" {
		port = "1051"
	}

	resolver, err := newResolver(conf)
	if err != nil {
		return nil, err
	}

	fqdns := make([]string, 0)
	for _, fqdn := range strings.Split(conf.FQDN, ",") {
		fqdn := strings.TrimSpace(fqdn)
		if fqdn == "" {
			continue
		}
		fqdns = append(fqdns, dns.Fqdn(fqdn))
	}

	ctx, cancel := context.WithCancel(context.Background())
	pool := &DNSPool{
		log:        conf.Logger,
		ctx:        ctx,
		cancel:     cancel,
		fqdns:      fqdns,
		resolver:   resolver,
		ownIP:      ip,
		ownPort:    port,
		onUpdate:   conf.OnUpdate,
		dataCenter: conf.DataCenter,
	}
	go pool.task()
	return pool, nil
}

func (p *DNSPool) peer(fqdn, ip string, ipv6 bool) PeerInfo {
	if ipv6 {
		ip = "[" + ip + "]"
	}
	dataCenter := ""
	if p.dataCenter != "" {
		dataCenter = fqdn
	}
	return PeerInfo{
		DataCenter:  dataCenter,
		GRPCAddress: ip + ":" + p.ownPort,
		IsOwner:     p.ownIP == ip,
	}
}

const (
	// defaultPollDelay is the DNS TTL-based poll interval in seconds used when
	// peers are found. The actual value is reduced to the minimum TTL returned
	// across all records so that cache expiry is respected.
	defaultPollDelay uint32 = 300
	// noPeersRetryDelay is the retry interval in seconds when no peers are
	// found (e.g. all pods restarting simultaneously). Matches CoreDNS's
	// default negative-cache TTL for Kubernetes services.
	noPeersRetryDelay uint32 = 5
)

func (p *DNSPool) task() {
	for {
		delay := defaultPollDelay
		var update []PeerInfo
		for _, fqdn := range p.fqdns {
			for _, t := range []uint16{dns.TypeA, dns.TypeAAAA} {
				ips, d, err := p.resolver.lookupHost(fqdn, t)
				if err != nil {
					p.log.Debugf("Error looking up %s (%s): %v", fqdn, dns.TypeToString[t], err)
					continue
				}
				if len(ips) == 0 {
					p.log.Debugf("No IPs found for %s (%s)", fqdn, dns.TypeToString[t])
					continue
				}

				delay = min(delay, d)
				for _, ip := range ips {
					update = append(update, p.peer(fqdn, ip.String(), t == dns.TypeAAAA))
				}
			}
		}

		if len(update) > 0 {
			p.onUpdate(update)
		} else {
			// Intentionally do not call onUpdate with an empty list. Clearing
			// the peer pickers during a transient DNS outage or rolling restart
			// would make this instance immediately unhealthy and unable to serve
			// requests. Keeping the stale peer list and retrying quickly is
			// safer: requests may fail transiently but the instance stays in
			// rotation and recovers as pods come back.
			delay = noPeersRetryDelay
			p.log.Warn("No peers found for DNS pool, trying again in 5 seconds")
		}

		p.log.Debug("DNS poll delay: ", delay)
		select {
		case <-p.ctx.Done():
			return
		case <-time.After(time.Duration(delay) * time.Second):
		}
	}
}

func (p *DNSPool) Close() {
	p.cancel()
}

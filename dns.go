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

func (r *DNSResolver) lookupHost(host string, dnsType uint16, delay uint32) ([]net.IP, uint32, error) {
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

	result := make([]net.IP, 0, len(in.Answer))
	for _, record := range in.Answer {
		switch r := record.(type) {
		case *dns.A:
			result = append(result, r.A)
		case *dns.AAAA:
			result = append(result, r.AAAA)
		}
		delay = min(delay, record.Header().Ttl)
	}

	return result, delay, nil
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

	Logger FieldLogger
}

type DNSPool struct {
	log      FieldLogger
	ctx      context.Context
	cancel   context.CancelFunc
	resolver *DNSResolver
	fqdns    []string // list of FQDNs to resolve
	ownIP    string
	ownPort  string
	onUpdate UpdateFunc
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
		fqdns = append(fqdns, fqdn)
	}

	ctx, cancel := context.WithCancel(context.Background())
	pool := &DNSPool{
		log:      conf.Logger,
		ctx:      ctx,
		cancel:   cancel,
		fqdns:    fqdns,
		resolver: resolver,
		ownIP:    ip,
		ownPort:  port,
		onUpdate: conf.OnUpdate,
	}
	go pool.task()
	return pool, nil
}

func (p *DNSPool) peer(fqdn, ip string, ipv6 bool) PeerInfo {
	if ipv6 {
		ip = "[" + ip + "]"
	}
	return PeerInfo{
		DataCenter:  fqdn,
		GRPCAddress: ip + ":" + p.ownPort,
		IsOwner:     p.ownIP == ip,
	}
}

func (p *DNSPool) task() {
	for {
		var delay uint32 = 300
		var update []PeerInfo
		for _, fqdn := range p.fqdns {
			for _, t := range []uint16{dns.TypeA, dns.TypeAAAA} {
				ips, d, err := p.resolver.lookupHost(fqdn, t, delay)
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
			p.log.Warn("No peers found for DNS pool")
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

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
	"fmt"
	"math/rand"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mailgun/holster/v4/clock"
	"github.com/mailgun/holster/v4/syncutil"
	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// RegisterRLSFunc registers Envoy's RateLimitService on every gRPC server,
// serving rate limits from the instance. The handler lives in the `envoy`
// subpackage so go-control-plane stays out of this package's import graph;
// cmd/gubernator sets this to envoy.Register. The returned collector is
// registered with the daemon's prometheus registry.
type RegisterRLSFunc func(servers []*grpc.Server, instance *V1Instance) prometheus.Collector

// EnvoyConfig holds the Envoy RateLimitService adapter settings. Policy for a
// domain is applied through the EnvoyPolicyV1 API; these are the defaults for
// domains with no policy.
type EnvoyConfig struct {
	// (Optional) Registers RateLimitService and EnvoyPolicyV1 on every gRPC server
	Enabled bool

	// (Optional) Algorithm for domains with no policy. Defaults to TOKEN_BUCKET
	Algorithm Algorithm

	// (Optional) Behavior bitflags for domains with no policy. Defaults to BATCHING
	Behavior Behavior

	// (Optional) What a domain with no policy does with a descriptor that carries
	// no limit. Defaults to DENY
	OnMissingLimit MissingLimitAction

	// (Optional) How often each peer pulls the full policy set from one random
	// peer. Defaults to 30s
	PolicySyncInterval time.Duration

	// (Required when Enabled) Registers the RateLimitService handler; see RegisterRLSFunc
	RegisterRLS RegisterRLSFunc
}

const defaultPolicySyncInterval = 30 * time.Second

// allBehaviors is every bit a policy may set; anything outside it is rejected.
const allBehaviors = Behavior_BATCHING | Behavior_NO_BATCHING | Behavior_GLOBAL | Behavior_DURATION_IS_GREGORIAN |
	Behavior_RESET_REMAINING | Behavior_MULTI_REGION | Behavior_DRAIN_OVER_LIMIT

var metricEnvoyPolicyVersion = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Name: "gubernator_envoy_policy_version",
	Help: "The version of the Envoy domain policy this peer holds, per domain. Tombstoned domains are removed.",
}, []string{"domain"})

// envoyPolicyEntry is one domain's slot in the store. A deleted entry is a
// tombstone that keeps its version so a stale apply cannot resurrect it.
type envoyPolicyEntry struct {
	policy  *DomainPolicy
	deleted bool
}

// envoyPolicyStore holds domain → policy with a single write path, merge.
// Readers take an immutable snapshot and never block on writers.
type envoyPolicyStore struct {
	snapshot atomic.Pointer[map[string]*envoyPolicyEntry]
	mutex    sync.Mutex
}

func newEnvoyPolicyStore() *envoyPolicyStore {
	s := &envoyPolicyStore{}
	s.snapshot.Store(&map[string]*envoyPolicyEntry{})
	return s
}

// Lookup returns the live policy for domain; false when absent or tombstoned.
func (s *envoyPolicyStore) Lookup(domain string) (*DomainPolicy, bool) {
	e, ok := (*s.snapshot.Load())[domain]
	if !ok || e.deleted {
		return nil, false
	}
	return e.policy, true
}

// Entries returns every entry, tombstones included, sorted by domain.
func (s *envoyPolicyStore) Entries() []*PeerPolicy {
	snap := *s.snapshot.Load()
	out := make([]*PeerPolicy, 0, len(snap))
	for _, e := range snap {
		out = append(out, &PeerPolicy{Policy: e.policy, Deleted: e.deleted})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Policy.Domain < out[j].Policy.Domain })
	return out
}

// outranks reports whether (version, origin) of a is strictly greater than b's.
func outranks(a, b *DomainPolicy) bool {
	if a.Version != b.Version {
		return a.Version > b.Version
	}
	return a.Origin > b.Origin
}

// merge installs incoming entries whose (version, origin) outranks the stored
// pair and discards the rest. It is the only mutation of the store. Entries
// must already carry a domain, a non-zero version and a non-empty origin.
func (s *envoyPolicyStore) merge(incoming []*PeerPolicy) {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	s.mergeLocked(incoming)
}

func (s *envoyPolicyStore) mergeLocked(incoming []*PeerPolicy) {
	old := *s.snapshot.Load()
	next := make(map[string]*envoyPolicyEntry, len(old)+len(incoming))
	for k, v := range old {
		next[k] = v
	}
	for _, in := range incoming {
		domain := in.Policy.Domain
		if cur, ok := next[domain]; ok && !outranks(in.Policy, cur.policy) {
			continue
		}
		next[domain] = &envoyPolicyEntry{policy: proto.Clone(in.Policy).(*DomainPolicy), deleted: in.Deleted}
		if in.Deleted {
			metricEnvoyPolicyVersion.DeleteLabelValues(domain)
		} else {
			metricEnvoyPolicyVersion.WithLabelValues(domain).Set(float64(in.Policy.Version))
		}
	}
	s.snapshot.Store(&next)
}

// stamp assigns each policy a version of max(now, stored+1) and the given
// origin, then merges. Stamping and merging share the lock so a local apply
// can never be stale.
func (s *envoyPolicyStore) stamp(policies []*PeerPolicy, origin string) {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	now := clock.Now().UnixMilli()
	snap := *s.snapshot.Load()
	// A domain repeated in one request still gets strictly increasing versions
	assigned := make(map[string]int64, len(policies))
	for _, p := range policies {
		version := now
		if cur, ok := snap[p.Policy.Domain]; ok && cur.policy.Version >= version {
			version = cur.policy.Version + 1
		}
		if prev, ok := assigned[p.Policy.Domain]; ok && prev >= version {
			version = prev + 1
		}
		assigned[p.Policy.Domain] = version
		p.Policy.Version = version
		p.Policy.Origin = origin
	}
	s.mergeLocked(policies)
}

// envoyPolicyManager owns the store and its propagation to peers.
type envoyPolicyManager struct {
	store        *envoyPolicyStore
	instance     *V1Instance
	conf         EnvoyConfig
	origin       string
	log          FieldLogger
	wg           syncutil.WaitGroup
	bootstrapped atomic.Bool
}

func newEnvoyPolicyManager(conf EnvoyConfig, instance *V1Instance) *envoyPolicyManager {
	m := &envoyPolicyManager{
		store:    newEnvoyPolicyStore(),
		instance: instance,
		conf:     conf,
		origin:   instance.conf.AdvertiseAddr,
		log:      instance.log,
	}
	if m.origin == "" {
		m.origin = instance.conf.InstanceID
	}
	m.runSync()
	return m
}

// runSync pulls the full policy set from one random peer every sync interval.
// Merge is idempotent and version-ordered, so pulling from any peer converges.
func (m *envoyPolicyManager) runSync() {
	ticker := clock.NewTicker(m.conf.PolicySyncInterval)
	m.wg.Until(func(done chan struct{}) bool {
		select {
		case <-ticker.C():
			m.pull(context.Background())
		case <-done:
			ticker.Stop()
			return false
		}
		return true
	})
}

func (m *envoyPolicyManager) Close() {
	m.wg.Stop()
}

// bootstrap pulls from a peer the first time this instance learns of one.
func (m *envoyPolicyManager) bootstrap() {
	if len(m.otherPeers()) == 0 || !m.bootstrapped.CompareAndSwap(false, true) {
		return
	}
	go m.pull(context.Background())
}

func (m *envoyPolicyManager) otherPeers() []*PeerClient {
	var out []*PeerClient
	for _, p := range m.instance.GetPeerList() {
		if !p.Info().IsOwner {
			out = append(out, p)
		}
	}
	return out
}

func (m *envoyPolicyManager) pull(ctx context.Context) {
	peers := m.otherPeers()
	if len(peers) == 0 {
		return
	}
	peer := peers[rand.Intn(len(peers))]
	ctx, cancel := context.WithTimeout(ctx, m.instance.conf.Behaviors.GlobalTimeout)
	defer cancel()
	resp, err := peer.GetPeerPolicies(ctx, &GetPeerPoliciesReq{})
	if err != nil {
		m.log.WithError(err).WithField("peer", peer.Info().GRPCAddress).
			Warn("while pulling envoy policies from peer")
		return
	}
	valid := make([]*PeerPolicy, 0, len(resp.Policies))
	for _, p := range resp.Policies {
		if err := validatePeerPolicy(p); err != nil {
			m.log.WithError(err).WithField("peer", peer.Info().GRPCAddress).
				Warn("ignoring invalid envoy policy from peer")
			continue
		}
		valid = append(valid, p)
	}
	m.store.merge(valid)
}

// broadcast sends entries to every other local peer in parallel, each bounded
// by GlobalTimeout, and returns the addresses that did not acknowledge.
func (m *envoyPolicyManager) broadcast(ctx context.Context, entries []*PeerPolicy) []string {
	var mutex sync.Mutex
	var unreachable []string
	req := &UpdatePeerPoliciesReq{Policies: entries}

	fan := syncutil.NewFanOut(m.instance.conf.Behaviors.GlobalPeerRequestsConcurrency)
	for _, peer := range m.otherPeers() {
		fan.Run(func(in any) error {
			peer := in.(*PeerClient)
			ctx, cancel := context.WithTimeout(ctx, m.instance.conf.Behaviors.GlobalTimeout)
			_, err := peer.UpdatePeerPolicies(ctx, req)
			cancel()
			if err != nil {
				m.log.WithError(err).WithField("peer", peer.Info().GRPCAddress).
					Warn("while broadcasting envoy policies to peer")
				mutex.Lock()
				unreachable = append(unreachable, peer.Info().GRPCAddress)
				mutex.Unlock()
			}
			return nil
		}, peer)
	}
	fan.Wait()
	sort.Strings(unreachable)
	return unreachable
}

func validateDomainPolicy(p *DomainPolicy) error {
	if p == nil {
		return fmt.Errorf("policy is required")
	}
	if p.Domain == "" {
		return fmt.Errorf("field 'domain' cannot be empty")
	}
	if _, ok := Algorithm_name[int32(p.Algorithm)]; !ok {
		return fmt.Errorf("domain %q: unknown algorithm %d", p.Domain, p.Algorithm)
	}
	if _, ok := MissingLimitAction_name[int32(p.OnMissingLimit)]; !ok {
		return fmt.Errorf("domain %q: unknown on_missing_limit %d", p.Domain, p.OnMissingLimit)
	}
	if Behavior(p.Behavior)&^allBehaviors != 0 {
		return fmt.Errorf("domain %q: unknown behavior bits in %d", p.Domain, p.Behavior)
	}
	return nil
}

func validatePeerPolicy(p *PeerPolicy) error {
	if p == nil {
		return fmt.Errorf("policy is required")
	}
	if err := validateDomainPolicy(p.Policy); err != nil {
		return err
	}
	if p.Policy.Version <= 0 {
		return fmt.Errorf("domain %q: field 'version' must be greater than zero", p.Policy.Domain)
	}
	if p.Policy.Origin == "" {
		return fmt.Errorf("domain %q: field 'origin' cannot be empty", p.Policy.Domain)
	}
	return nil
}

// ResolveEnvoyPolicy returns the policy for domain, or one built from the
// global defaults when the domain has none. The result is a copy.
func (s *V1Instance) ResolveEnvoyPolicy(domain string) *DomainPolicy {
	if s.envoy != nil {
		if p, ok := s.envoy.store.Lookup(domain); ok {
			return proto.Clone(p).(*DomainPolicy)
		}
	}
	return &DomainPolicy{
		Domain:         domain,
		Algorithm:      s.conf.Envoy.Algorithm,
		Behavior:       int32(s.conf.Envoy.Behavior),
		OnMissingLimit: s.conf.Envoy.OnMissingLimit,
	}
}

// ApplyPolicies validates every entry, stamps each with a version and this
// peer's origin, merges locally, then broadcasts to every other local peer.
func (s *V1Instance) ApplyPolicies(ctx context.Context, r *ApplyPoliciesReq) (*ApplyPoliciesResp, error) {
	entries := make([]*PeerPolicy, 0, len(r.Policies))
	for _, p := range r.Policies {
		if err := validateDomainPolicy(p); err != nil {
			return nil, status.Error(codes.InvalidArgument, err.Error())
		}
		entries = append(entries, &PeerPolicy{Policy: proto.Clone(p).(*DomainPolicy)})
	}
	s.envoy.store.stamp(entries, s.envoy.origin)

	resp := &ApplyPoliciesResp{UnreachablePeers: s.envoy.broadcast(ctx, entries)}
	for _, e := range entries {
		resp.Applied = append(resp.Applied, e.Policy)
	}
	return resp, nil
}

// DeletePolicies tombstones each domain with a fresh version and broadcasts
// the tombstones like an apply.
func (s *V1Instance) DeletePolicies(ctx context.Context, r *DeletePoliciesReq) (*DeletePoliciesResp, error) {
	entries := make([]*PeerPolicy, 0, len(r.Domains))
	for _, domain := range r.Domains {
		if domain == "" {
			return nil, status.Error(codes.InvalidArgument, "field 'domain' cannot be empty")
		}
		entries = append(entries, &PeerPolicy{Policy: &DomainPolicy{Domain: domain}, Deleted: true})
	}
	s.envoy.store.stamp(entries, s.envoy.origin)
	return &DeletePoliciesResp{UnreachablePeers: s.envoy.broadcast(ctx, entries)}, nil
}

// ListPolicies returns this peer's live policies; tombstones are omitted.
func (s *V1Instance) ListPolicies(_ context.Context, _ *ListPoliciesReq) (*ListPoliciesResp, error) {
	resp := &ListPoliciesResp{}
	for _, e := range s.envoy.store.Entries() {
		if !e.Deleted {
			resp.Policies = append(resp.Policies, e.Policy)
		}
	}
	return resp, nil
}

// UpdatePeerPolicies merges entries broadcast by another peer. Stale entries
// are discarded silently; malformed entries fail the call.
func (s *V1Instance) UpdatePeerPolicies(_ context.Context, r *UpdatePeerPoliciesReq) (*UpdatePeerPoliciesResp, error) {
	if s.envoy == nil {
		return nil, status.Error(codes.Unimplemented, "envoy rate limit service is not enabled on this peer")
	}
	for _, p := range r.Policies {
		if err := validatePeerPolicy(p); err != nil {
			return nil, status.Error(codes.InvalidArgument, err.Error())
		}
	}
	s.envoy.store.merge(r.Policies)
	return &UpdatePeerPoliciesResp{}, nil
}

// GetPeerPolicies returns every entry this peer holds, tombstones included.
func (s *V1Instance) GetPeerPolicies(_ context.Context, _ *GetPeerPoliciesReq) (*GetPeerPoliciesResp, error) {
	if s.envoy == nil {
		return nil, status.Error(codes.Unimplemented, "envoy rate limit service is not enabled on this peer")
	}
	return &GetPeerPoliciesResp{Policies: s.envoy.store.Entries()}, nil
}

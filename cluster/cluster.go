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

package cluster

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand"

	"github.com/gubernator-io/gubernator/v3"
	"github.com/kapetan-io/errors"
	"github.com/kapetan-io/tackle/clock"
)

const (
	DataCenterNone = ""
	DataCenterOne  = "datacenter-1"
	DataCenterTwo  = "datacenter-2"
)

var daemons []*gubernator.Daemon
var peers []gubernator.PeerInfo

// GetRandomPeerOptions returns gubernator.ClientOptions for a random peer in the cluster
func GetRandomPeerOptions(dc string) gubernator.ClientOptions {
	info := GetRandomPeerInfo(dc)
	return gubernator.WithNoTLS(info.HTTPAddress)
}

// GetRandomPeerInfo returns a random peer from the cluster
func GetRandomPeerInfo(dc string) gubernator.PeerInfo {
	var local []gubernator.PeerInfo

	for _, p := range peers {
		if p.DataCenter == dc {
			local = append(local, p)
		}
	}

	if len(local) == 0 {
		panic(fmt.Sprintf("failed to find random peer for dc '%s'", dc))
	}

	return local[rand.Intn(len(local))]
}

// GetRandomDaemon returns a random daemon from the cluster
func GetRandomDaemon(dc string) *gubernator.Daemon {
	var local []*gubernator.Daemon

	for _, d := range daemons {
		if d.PeerInfo.DataCenter == dc {
			local = append(local, d)
		}
	}

	if len(local) == 0 {
		panic(fmt.Sprintf("failed to find random daemon for dc '%s'", dc))
	}

	return local[rand.Intn(len(local))]
}

// GetPeers returns a list of all peers in the cluster
func GetPeers() []gubernator.PeerInfo {
	return peers
}

// GetDaemons returns a list of all daemons in the cluster
func GetDaemons() []*gubernator.Daemon {
	return daemons
}

// PeerAt returns a specific peer
func PeerAt(idx int) gubernator.PeerInfo {
	return peers[idx]
}

// FindOwningPeer finds the peer which owns the rate limit with the provided name and unique key
func FindOwningPeer(name, key string) (gubernator.PeerInfo, error) {
	p, err := daemons[0].Service.GetPeer(context.Background(), name+"_"+key)
	if err != nil {
		return gubernator.PeerInfo{}, err
	}
	return p.Info(), nil
}

// FindOwningDaemon finds the daemon which owns the rate limit with the provided name and unique key
func FindOwningDaemon(name, key string) (*gubernator.Daemon, error) {
	p, err := daemons[0].Service.GetPeer(context.Background(), name+"_"+key)
	if err != nil {
		return &gubernator.Daemon{}, err
	}

	for i, d := range daemons {
		if d.Config().HTTPListenAddress == p.Info().HTTPAddress {
			return daemons[i], nil
		}
	}
	return &gubernator.Daemon{}, errors.New("unable to find owning daemon")
}

// ListNonOwningDaemons returns a list of daemons in the cluster that do not own the rate limit
// for the name and key provided.
func ListNonOwningDaemons(name, key string) ([]*gubernator.Daemon, error) {
	owner, err := FindOwningDaemon(name, key)
	if err != nil {
		return []*gubernator.Daemon{}, err
	}

	var daemons []*gubernator.Daemon
	for _, d := range GetDaemons() {
		if d.Config().HTTPListenAddress != owner.Config().HTTPListenAddress {
			daemons = append(daemons, d)
		}
	}
	return daemons, nil
}

// DaemonAt returns a specific daemon
func DaemonAt(idx int) *gubernator.Daemon {
	return daemons[idx]
}

// NumOfDaemons returns the number of instances
func NumOfDaemons() int {
	return len(daemons)
}

// Start a local cluster of gubernator servers
func Start(numInstances int) error {
	// Ideally, we should let the socket choose the port, but then
	// some things like the logger will not be set correctly.
	var peers []gubernator.PeerInfo
	port := 1111
	for i := 0; i < numInstances; i++ {
		peers = append(peers, gubernator.PeerInfo{
			HTTPAddress: fmt.Sprintf("localhost:%d", port),
		})
		port += 1
	}
	return StartWith(peers)
}

// Restart the cluster
func Restart(ctx context.Context) error {
	for i := 0; i < len(daemons); i++ {
		if err := daemons[i].Close(ctx); err != nil {
			return err
		}
		if err := daemons[i].Start(ctx); err != nil {
			return err
		}
		daemons[i].SetPeers(peers)
	}
	return nil
}

// StartWith a local cluster with specific addresses
func StartWith(localPeers []gubernator.PeerInfo, opts ...Option) error {
	for _, peer := range localPeers {
		ctx, cancel := context.WithTimeout(context.Background(), clock.Second*10)
		cfg := gubernator.DaemonConfig{
			Logger:            slog.Default().With("instance", peer.HTTPAddress),
			InstanceID:        peer.HTTPAddress,
			HTTPListenAddress: peer.HTTPAddress,
			AdvertiseAddress:  peer.HTTPAddress,
			DataCenter:        peer.DataCenter,
			CacheProvider:     "otter",
			Behaviors: gubernator.BehaviorConfig{
				// Suitable for testing but not production
				GlobalSyncWait: clock.Millisecond * 50,
				GlobalTimeout:  clock.Second * 5,
				BatchTimeout:   clock.Second * 5,
			},
		}

		for _, opt := range opts {
			opt.Apply(&cfg)
		}
		d, err := gubernator.SpawnDaemon(ctx, cfg)
		cancel()
		if err != nil {
			return fmt.Errorf("while starting server for addr '%s': %w", peer.HTTPAddress, err)
		}

		// Add the peers and daemons to the package level variables
		peers = append(peers, d.PeerInfo)
		daemons = append(daemons, d)
	}

	// Tell each instance about the other peers
	for _, d := range daemons {
		d.SetPeers(peers)
	}
	return nil
}

// Stop all daemons in the cluster
func Stop(ctx context.Context) {
	for _, d := range daemons {
		_ = d.Close(ctx)
	}
	peers = nil
	daemons = nil
}

type Option interface {
	Apply(cfg *gubernator.DaemonConfig)
}

type eventChannelOption struct {
	eventChannel chan<- gubernator.HitEvent
}

func (o *eventChannelOption) Apply(cfg *gubernator.DaemonConfig) {
	cfg.EventChannel = o.eventChannel
}

// WithEventChannel sets EventChannel to Gubernator config.
func WithEventChannel(eventChannel chan<- gubernator.HitEvent) Option {
	return &eventChannelOption{eventChannel: eventChannel}
}
